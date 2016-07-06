// Package tracer implements a Dapper-style tracing system. It is
// compatible with the Open Tracing specification.
//
// Errors and logging
//
// The instrumentation is defensive and will never purposefully panic.
// At the same time, most functions do not return errors, because
// they'll be called by automatic instrumentation, hidden from the
// user. Instead, errors will be logged. The logger can be changed by
// assigning to the Logger field in Tracer.
package tracer

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"log"
	"reflect"
	"time"

	"github.com/opentracing/opentracing-go"
)

const (
	FlagSampled = 1 << iota
)

// A Logger logs messages.
type Logger interface {
	// Printf logs a single message, given a format and values. The
	// format is documented in the fmt package.
	Printf(format string, values ...interface{})
}

type defaultLogger struct{}

func (defaultLogger) Printf(format string, values ...interface{}) {
	log.Printf(format, values...)
}

// valueType returns the broad categorization of a value's type and
// whether it is permitted as a payload.
func valueType(v interface{}) (string, bool) {
	if v == nil {
		return "", true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return "", false
	}
	switch rv.Type().Kind() {
	case reflect.Bool:
		return "boolean", true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return "number", true
	case reflect.String:
		return "string", true
	}
	return "", false
}

// A RawTrace contains all the data associated with a trace.
type RawTrace struct {
	TraceID uint64
	Spans   []RawSpan
}

// Span is an implementation of the Open Tracing Span interface.
type Span struct {
	tracer *Tracer
	RawSpan
}

// A RawSpan contains all the data associated with a span.
type RawSpan struct {
	ServiceName   string
	SpanID        uint64
	ParentID      uint64
	TraceID       uint64
	OperationName string
	StartTime     time.Time
	FinishTime    time.Time
	Flags         uint64

	Tags    map[string]interface{}
	Baggage map[string]string
	Logs    []opentracing.LogData
}

// Sampled reports whether this span was sampled.
func (sp *Span) Sampled() bool {
	return (sp.Flags & FlagSampled) > 0
}

// SetOperationName sets the span's operation name.
func (sp *Span) SetOperationName(name string) opentracing.Span {
	sp.OperationName = name
	return sp
}

func (sp *Span) SetTag(key string, value interface{}) opentracing.Span {
	if !sp.Sampled() {
		return sp
	}
	if _, ok := valueType(value); !ok {
		sp.tracer.Logger.Printf("unsupported tag value type for tag %q: %T", key, value)
		return sp
	}
	if sp.Tags == nil {
		sp.Tags = map[string]interface{}{}
	}
	sp.Tags[key] = value
	return sp
}

func (sp *Span) Finish() {
	if !sp.Sampled() {
		return
	}
	sp.FinishWithOptions(opentracing.FinishOptions{})
}

func (sp *Span) FinishWithOptions(opts opentracing.FinishOptions) {
	if !sp.Sampled() {
		return
	}
	if opts.FinishTime.IsZero() {
		opts.FinishTime = time.Now()
	}
	sp.FinishTime = opts.FinishTime
	for _, log := range opts.BulkLogData {
		sp.Log(log)
	}
	if err := sp.tracer.storer.Store(sp.RawSpan); err != nil {
		sp.tracer.Logger.Printf("error while storing tracing span: %s", err)
	}
}

func (sp *Span) LogEvent(event string) {
	if !sp.Sampled() {
		return
	}
	sp.Log(opentracing.LogData{
		Event: event,
	})
}

func (sp *Span) LogEventWithPayload(event string, payload interface{}) {
	if !sp.Sampled() {
		return
	}
	sp.Log(opentracing.LogData{
		Event:   event,
		Payload: payload,
	})
}

func (sp *Span) Log(data opentracing.LogData) {
	if !sp.Sampled() {
		return
	}
	if _, ok := valueType(data.Payload); !ok {
		sp.tracer.Logger.Printf("unsupported log payload type for event %q: %T", data.Event, data.Payload)
		return
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = time.Now()
	}
	sp.Logs = append(sp.Logs, data)
}

func (sp *Span) SetBaggageItem(key, value string) opentracing.Span {
	if !sp.Sampled() {
		return sp
	}
	sp.Baggage[key] = value
	return sp
}

func (sp *Span) BaggageItem(key string) string {
	return sp.Baggage[key]
}

func (sp *Span) ForeachBaggageItem(handler func(k, v string) bool) {
	for k, v := range sp.Baggage {
		if !handler(k, v) {
			return
		}
	}
}

func (sp *Span) Tracer() opentracing.Tracer {
	return sp.tracer
}

// Tracer is an implementation of the Open Tracing Tracer interface.
type Tracer struct {
	ServiceName string
	Logger      Logger
	Sampler     Sampler

	storer      Storer
	idGenerator IDGenerator
}

func NewTracer(serviceName string, storer Storer, idGenerator IDGenerator) *Tracer {
	return &Tracer{
		ServiceName: serviceName,
		Logger:      defaultLogger{},
		Sampler:     NewConstSampler(true),
		storer:      storer,
		idGenerator: idGenerator,
	}
}

func (tr *Tracer) StartSpan(operationName string) opentracing.Span {
	return tr.StartSpanWithOptions(opentracing.StartSpanOptions{
		OperationName: operationName,
	})
}

func (tr *Tracer) StartSpanWithOptions(opts opentracing.StartSpanOptions) opentracing.Span {
	if opts.StartTime.IsZero() {
		opts.StartTime = time.Now()
	}

	id := tr.idGenerator.GenerateID()
	sp := &Span{
		tracer: tr,
		RawSpan: RawSpan{
			ServiceName:   tr.ServiceName,
			OperationName: opts.OperationName,
			SpanID:        id,
			TraceID:       id,
			StartTime:     opts.StartTime,
		},
	}
	if opts.Parent != nil {
		parent, ok := opts.Parent.(*Span)
		if !ok {
			panic("parent span must be of type *Span")
		}
		sp.ParentID = parent.SpanID
		sp.TraceID = parent.TraceID
		sp.Flags = parent.Flags
	} else {
		if tr.Sampler.Sample(id) {
			sp.Flags |= FlagSampled
		}
	}
	return sp
}

func idToHex(id uint64) string {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, id)
	return hex.EncodeToString(b)
}

func idFromHex(s string) uint64 {
	b, _ := hex.DecodeString(s)
	return binary.BigEndian.Uint64(b)
}

func (tr *Tracer) Inject(sp opentracing.Span, format interface{}, carrier interface{}) error {
	span, ok := sp.(*Span)
	if !ok {
		return opentracing.ErrInvalidSpan
	}
	injecter, ok := injecters[format]
	if !ok {
		return opentracing.ErrUnsupportedFormat
	}
	return injecter(span, carrier)
}

func (tr *Tracer) Join(operationName string, format interface{}, carrier interface{}) (opentracing.Span, error) {
	joiner, ok := joiners[format]
	if !ok {
		return nil, opentracing.ErrUnsupportedFormat
	}
	context, err := joiner(carrier)
	if err != nil {
		return nil, err
	}

	sp := &Span{
		tracer: tr,
		RawSpan: RawSpan{
			TraceID:  context.TraceID,
			SpanID:   context.SpanID,
			ParentID: context.ParentID,
			Baggage:  context.Baggage,
			Flags:    context.Flags,
		},
	}
	return opentracing.StartChildSpan(sp, operationName), nil
}

// IDGenerator generates IDs for traces and spans. The ID with value 0
// is reserved to mean "no parent span" and should not be generated.
type IDGenerator interface {
	GenerateID() uint64
}

// A Storer stores a finished span. "Storing" a span may either mean
// saving it in a storage engine, or sending it to a remote
// collector.
//
// If a span with the same ID and the same trace ID already exists,
// the existing and new spans should be merged into one span.
//
// Because spans are only stored once they're done, children will be
// stored before their parents.
type Storer interface {
	Store(sp RawSpan) error
}

// A Queryer is a backend that allows fetching traces and spans by ID
// or via a more advanced query.
type Queryer interface {
	// TraceWithID returns a trace with a specific ID.
	TraceWithID(id uint64) (RawTrace, error)
	// SpanWithID returns a span with a specific ID.
	SpanWithID(id uint64) (RawSpan, error)
	// QueryTraces returns all traces that match a query.
	QueryTraces(q Query) ([]RawTrace, error)
}

// QueryTag describes a single tag or log entry that should be queried
// for.
type QueryTag struct {
	// The key of the tag.
	Key string
	// The value of the tag.
	Value string
	// Whether the value should be checked for.
	CheckValue bool
}

type Query struct {
	StartTime     time.Time
	FinishTime    time.Time
	OperationName string
	MinDuration   time.Duration
	MaxDuration   time.Duration
	AndTags       []QueryTag
	OrTags        []QueryTag
}

var _ IDGenerator = RandomID{}

// RandomID generates random IDs by using crypto/rand.
type RandomID struct{}

// GenerateID generates an ID.
func (RandomID) GenerateID() uint64 {
	b := make([]byte, 8)
	for {
		_, _ = rand.Read(b)
		x := binary.BigEndian.Uint64(b)
		if x != 0 {
			return x
		}
	}
}
