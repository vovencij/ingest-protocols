package signalfx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	jaegerpb "github.com/jaegertracing/jaeger/model"
	"github.com/mailru/easyjson"
	"github.com/signalfx/golib/v3/datapoint/dpsink"
	"github.com/signalfx/golib/v3/log"
	"github.com/signalfx/golib/v3/sfxclient"
	"github.com/signalfx/golib/v3/trace"
	"github.com/signalfx/golib/v3/trace/translator"
	"github.com/signalfx/golib/v3/web"
	signalfxformat "github.com/signalfx/ingest-protocols/protocol/signalfx/format"
	splunksapm "github.com/signalfx/sapm-proto/gen"
)

const (
	// DefaultTracePathV1 is the default listen path
	DefaultTracePathV1 = "/v1/trace"
	// ZipkinTracePathV1 adds /api/v1/spans endpoint
	ZipkinTracePathV1 = "/api/v1/spans"
	// ZipkinTracePathV2 adds /api/vw/spans endpoint
	ZipkinTracePathV2 = "/api/v2/spans"
	// ZipkinV1 is a constant used for protocol naming
	ZipkinV1 = "zipkin_json_v1"
)

// Constants as variables so it is easy to get a pointer to them
var (
	trueVar = true

	ClientKind   = "CLIENT"
	ServerKind   = "SERVER"
	ProducerKind = "PRODUCER"
	ConsumerKind = "CONSUMER"
)

// InputSpan is an alias
type InputSpan signalfxformat.InputSpan

func (is *InputSpan) isDefinitelyZipkinV2() bool {
	// The presence of the "kind" field, tags, or local/remote endpoints is a
	// dead giveaway that this is a Zipkin v2 span, so shortcut the whole
	// process and return it as an optimization.  If it doesn't have any of
	// those things it could still be a V2 span since none of them are strictly
	// required to be there.
	return is.Span.Kind != nil || len(is.Span.Tags) > 0 || is.Span.LocalEndpoint != nil || is.Span.RemoteEndpoint != nil
}

// V2AnnotationsToJaegerLogs converts ZipkinV2 or SignalFx annotations to jaeger logs
func (is *InputSpan) v2AnnotationsToJaegerLogs(annotations []*signalfxformat.InputAnnotation) []jaegerpb.Log {
	logs := make([]jaegerpb.Log, 0, len(annotations))
	for _, ann := range annotations {
		if ann.Value == nil {
			continue
		}
		l := jaegerpb.Log{}
		if ann.Timestamp != nil {
			l.Timestamp = translator.TimeFromMicrosecondsSinceEpoch(int64(*ann.Timestamp))
		}
		var err error
		l.Fields, err = translator.FieldsFromJSONString(*ann.Value)
		if err != nil {
			// We are choosing to drop the log line and not the span under the philosophy that
			// for users it is better to miss a  tag or logline instead of missing the entire operation.
			continue
		}
		logs = append(logs, l)
	}
	return logs
}

var errSpansCannotHaveBinaryAnnotations = errors.New("span cannot have binaryAnnotations with Zipkin V2 fields")

// asZipkinV2 shortcuts the span conversion process and treats the InputSpan as
// ZipkinV2 and returns that span directly.
func (is *InputSpan) fromZipkinV2() (*trace.Span, error) {
	// Do some basic validation
	if len(is.BinaryAnnotations) > 0 {
		return nil, errSpansCannotHaveBinaryAnnotations
	}

	if len(is.Annotations) > 0 {
		is.Span.Annotations = make([]*trace.Annotation, len(is.Annotations))
		for i := range is.Annotations {
			is.Span.Annotations[i] = is.Annotations[i].ToV2()
		}
	}
	is.Span.ParentID = normalizeParentSpanID(is.Span.ParentID)

	return &is.Span, nil
}

// JaegerFromZipkinV2 shortcuts the span conversion process and treats the InputSpan as
// ZipkinV2 and returns that span directly as SAPM.
func (is *InputSpan) JaegerFromZipkinV2() (*jaegerpb.Span, error) {
	// Do some basic validation
	if len(is.BinaryAnnotations) > 0 {
		return nil, errors.New("span cannot have binaryAnnotations with Zipkin V2 fields")
	}

	var err error
	var span = &jaegerpb.Span{Process: &jaegerpb.Process{}}

	span.SpanID, err = jaegerpb.SpanIDFromString(is.ID)
	if err == nil {
		span.TraceID, err = jaegerpb.TraceIDFromString(is.TraceID)
		if err == nil {

			if is.Name != nil {
				span.OperationName = *is.Name
			}

			if is.Duration != nil {
				span.Duration = translator.DurationFromMicroseconds(int64(*is.Duration))
			}

			if is.Timestamp != nil {
				span.StartTime = translator.TimeFromMicrosecondsSinceEpoch(int64(*is.Timestamp))
			}

			if is.Debug != nil && *is.Debug {
				span.Flags.SetDebug()
			}

			span.Tags, span.Process.Tags = translator.SFXTagsToJaegerTags(is.Tags, is.RemoteEndpoint, is.Kind)

			translator.GetLocalEndpointInfo(&is.Span, span)

			is.Span.ParentID = normalizeParentSpanID(is.Span.ParentID)

			if is.Span.ParentID != nil {
				// only add parent/child reference if the parent id can be parsed
				if parentID, err := jaegerpb.SpanIDFromString(*is.Span.ParentID); err == nil {
					span.References = append(span.References, jaegerpb.SpanRef{
						TraceID: span.TraceID,
						SpanID:  parentID,
						RefType: jaegerpb.SpanRefType_CHILD_OF,
					})
				}
			}

			span.Logs = is.v2AnnotationsToJaegerLogs(is.Annotations)
		}
	}

	return span, err
}

// asTraceSpan should be used when we are not sure that the InputSpan is
// already in Zipkin V2 format.  It returns a slice of our SignalFx span
// object, which is equivalent to a Zipkin V2 span.  A single span in Zipkin v1
// can contain multiple v2 spans because the annotations and binary annotations
// contain endpoints.  This would also work for Zipkin V2 spans, it just
// involves a lot more processing.  The conversion code was mostly ported from
// https://github.com/openzipkin/zipkin/blob/2.8.4/zipkin/src/main/java/zipkin/internal/V2SpanConverter.java
func (is *InputSpan) fromZipkinV1() ([]*trace.Span, error) {
	if is.Span.Tags == nil {
		is.Span.Tags = map[string]string{}
	}

	is.Span.ParentID = normalizeParentSpanID(is.Span.ParentID)

	spanCopy := is.Span
	spanBuilder := &spanBuilder{
		spans: []*trace.Span{&spanCopy},
	}
	spanBuilder.processAnnotations(is)
	if err := spanBuilder.processBinaryAnnotations(is); err != nil {
		return nil, err
	}

	return spanBuilder.spans, nil
}

func (is *InputSpan) endTimestampReflectsSpanDuration(end *signalfxformat.InputAnnotation) bool {
	return end != nil && is.Timestamp != nil && is.Duration != nil && end.Timestamp != nil &&
		*is.Timestamp+*is.Duration == *end.Timestamp
}

type spanBuilder struct {
	spans                          []*trace.Span
	cs, sr, ss, cr, ms, mr, ws, wr *signalfxformat.InputAnnotation
}

func (sb *spanBuilder) addSpanForEndpoint(is *InputSpan, e *trace.Endpoint) *trace.Span {
	s := is.Span
	s.LocalEndpoint = e
	s.Tags = map[string]string{}

	sb.spans = append(sb.spans, &s)
	return &s
}

func (sb *spanBuilder) spanForEndpoint(is *InputSpan, e *trace.Endpoint) *trace.Span {
	if e == nil {
		// Allocate missing endpoint data to first span.  For a Zipkin v2
		// span this will be the only one.
		return sb.spans[0]
	}

	for i := range sb.spans {
		next := sb.spans[i]
		if next.LocalEndpoint == nil {
			next.LocalEndpoint = e
			return next
		} else if closeEnough(next.LocalEndpoint, e) {
			return next
		}
	}

	return sb.addSpanForEndpoint(is, e)
}

func (sb *spanBuilder) processAnnotations(is *InputSpan) {
	sb.pullOutSpecialAnnotations(is)
	sb.fillInStartAnnotations(is)

	if sb.cs != nil && sb.sr != nil {
		sb.fillInMissingTimings(is)
	} else if sb.cs != nil && sb.cr != nil {
		sb.maybeTimestampDuration(sb.cs, sb.cr, is)
	} else if sb.sr != nil && sb.ss != nil {
		sb.maybeTimestampDuration(sb.sr, sb.ss, is)
	} else { // otherwise, the span is incomplete. revert special-casing
		sb.handleIncompleteSpan(is)
	}

	// Span v1 format did not have a shared flag. By convention, span.timestamp being absent
	// implied shared. When we only see the server-side, carry this signal over.
	if sb.cs == nil && (sb.sr != nil && is.Timestamp == nil) {
		sb.spanForEndpoint(is, sb.sr.Endpoint).Shared = &trueVar
	}

	sb.handleMessageQueueAnnotations(is)
}

func (sb *spanBuilder) pullOutSpecialAnnotations(is *InputSpan) {
	for i := range is.Annotations {
		anno := is.Annotations[i]

		span := sb.spanForEndpoint(is, anno.Endpoint)

		var processed bool
		// core annotations require an endpoint. Don't give special treatment when that's missing
		if anno.Value != nil && len(*anno.Value) == 2 && anno.Endpoint != nil {
			processed = sb.handleSpecialAnnotation(anno, span)
		} else {
			processed = false
		}

		if !processed {
			span.Annotations = append(span.Annotations, &trace.Annotation{
				Timestamp: signalfxformat.GetPointerToInt64(anno.Timestamp),
				Value:     anno.Value,
			})
		}
	}
}

func (sb *spanBuilder) handleSpecialAnnotation(anno *signalfxformat.InputAnnotation, span *trace.Span) bool {
	switch *anno.Value {
	case "cs":
		span.Kind = &ClientKind
		sb.cs = anno
	case "sr":
		span.Kind = &ServerKind
		sb.sr = anno
	case "ss":
		span.Kind = &ServerKind
		sb.ss = anno
	case "cr":
		span.Kind = &ClientKind
		sb.cr = anno
	case "ms":
		span.Kind = &ProducerKind
		sb.ms = anno
	case "mr":
		span.Kind = &ConsumerKind
		sb.mr = anno
	case "ws":
		sb.ws = anno
	case "wr":
		sb.wr = anno
	default:
		return false
	}
	return true
}

func (sb *spanBuilder) fillInStartAnnotations(is *InputSpan) {
	// When bridging between event and span model, you can end up missing a start annotation
	if sb.cs == nil && is.endTimestampReflectsSpanDuration(sb.cr) {
		val := "cs"
		sb.cs = &signalfxformat.InputAnnotation{
			Timestamp: is.Timestamp,
			Value:     &val,
			Endpoint:  sb.cr.Endpoint,
		}
	}
	if sb.sr == nil && is.endTimestampReflectsSpanDuration(sb.ss) {
		val := "sr"
		sb.sr = &signalfxformat.InputAnnotation{
			Timestamp: is.Timestamp,
			Value:     &val,
			Endpoint:  sb.ss.Endpoint,
		}
	}
}

func (sb *spanBuilder) fillInMissingTimings(is *InputSpan) {
	// in a shared span, the client side owns span duration by annotations or explicit timestamp
	sb.maybeTimestampDuration(sb.cs, sb.cr, is)

	// special-case loopback: We need to make sure on loopback there are two span2s
	client := sb.spanForEndpoint(is, sb.cs.Endpoint)

	var server *trace.Span
	if closeEnough(sb.cs.Endpoint, sb.sr.Endpoint) {
		client.Kind = &ClientKind
		// fork a new span for the server side
		server = sb.addSpanForEndpoint(is, sb.sr.Endpoint)
		server.Kind = &ServerKind
	} else {
		server = sb.spanForEndpoint(is, sb.sr.Endpoint)
	}

	// the server side is smaller than that, we have to read annotations to find out
	server.Shared = &trueVar
	server.Timestamp = signalfxformat.GetPointerToInt64(sb.sr.Timestamp)
	if sb.ss != nil && sb.ss.Timestamp != nil && sb.sr.Timestamp != nil {
		ts := *sb.ss.Timestamp - *sb.sr.Timestamp
		server.Duration = signalfxformat.GetPointerToInt64(&ts)
	}
	if sb.cr == nil && is.Duration == nil {
		client.Duration = nil
	}
}

func (sb *spanBuilder) handleIncompleteSpan(is *InputSpan) {
	for i := range sb.spans {
		next := sb.spans[i]
		if next.Kind != nil && *next.Kind == ClientKind {
			if sb.cs != nil {
				next.Timestamp = signalfxformat.GetPointerToInt64(sb.cs.Timestamp)
			}
			if sb.cr != nil {
				next.Annotations = append(next.Annotations, &trace.Annotation{
					Timestamp: signalfxformat.GetPointerToInt64(sb.cr.Timestamp),
					Value:     sb.cr.Value,
				})
			}
		} else if next.Kind != nil && *next.Kind == ServerKind {
			if sb.sr != nil {
				next.Timestamp = signalfxformat.GetPointerToInt64(sb.sr.Timestamp)
			}
			if sb.ss != nil {
				next.Annotations = append(next.Annotations, &trace.Annotation{
					Timestamp: signalfxformat.GetPointerToInt64(sb.ss.Timestamp),
					Value:     sb.ss.Value,
				})
			}
		}

		sb.fillInTimingsOnFirstSpan(is)
	}
}

func (sb *spanBuilder) fillInTimingsOnFirstSpan(is *InputSpan) {
	if is.Timestamp != nil {
		sb.spans[0].Timestamp = signalfxformat.GetPointerToInt64(is.Timestamp)
		sb.spans[0].Duration = signalfxformat.GetPointerToInt64(is.Duration)
	}
}

func (sb *spanBuilder) handleMessageQueueAnnotations(is *InputSpan) {
	// ms and mr are not supposed to be in the same span, but in case they are..
	if sb.ms != nil && sb.mr != nil {
		sb.handleBothMSAndMR(is)
	} else if sb.ms != nil {
		sb.maybeTimestampDuration(sb.ms, sb.ws, is)
	} else if sb.mr != nil {
		if sb.wr != nil {
			sb.maybeTimestampDuration(sb.wr, sb.mr, is)
		} else {
			sb.maybeTimestampDuration(sb.mr, nil, is)
		}
	} else {
		if sb.ws != nil {
			span := sb.spanForEndpoint(is, sb.ws.Endpoint)
			span.Annotations = append(span.Annotations, &trace.Annotation{
				Timestamp: signalfxformat.GetPointerToInt64(sb.ws.Timestamp),
				Value:     sb.ws.Value,
			})
		}
		if sb.wr != nil {
			span := sb.spanForEndpoint(is, sb.wr.Endpoint)
			span.Annotations = append(span.Annotations, &trace.Annotation{
				Timestamp: signalfxformat.GetPointerToInt64(sb.wr.Timestamp),
				Value:     sb.wr.Value,
			})
		}
	}
}

func (sb *spanBuilder) handleBothMSAndMR(is *InputSpan) {
	// special-case loopback: We need to make sure on loopback there are two span2s
	producer := sb.spanForEndpoint(is, sb.ms.Endpoint)
	var consumer *trace.Span
	if closeEnough(sb.ms.Endpoint, sb.mr.Endpoint) {
		producer.Kind = &ProducerKind
		// fork a new span for the consumer side
		consumer = sb.addSpanForEndpoint(is, sb.mr.Endpoint)
		consumer.Kind = &ConsumerKind
	} else {
		consumer = sb.spanForEndpoint(is, sb.mr.Endpoint)
	}

	consumer.Shared = &trueVar
	if sb.wr != nil && sb.mr.Timestamp != nil && sb.wr.Timestamp != nil {
		consumer.Timestamp = signalfxformat.GetPointerToInt64(sb.wr.Timestamp)
		ts := *sb.mr.Timestamp - *sb.wr.Timestamp
		consumer.Duration = signalfxformat.GetPointerToInt64(&ts)
	} else {
		consumer.Timestamp = signalfxformat.GetPointerToInt64(sb.mr.Timestamp)
	}

	producer.Timestamp = signalfxformat.GetPointerToInt64(sb.ms.Timestamp)
	if sb.ws != nil && sb.ws.Timestamp != nil && sb.ms.Timestamp != nil {
		ts := *sb.ws.Timestamp - *sb.ms.Timestamp
		producer.Duration = signalfxformat.GetPointerToInt64(&ts)
	}
}

func (sb *spanBuilder) maybeTimestampDuration(begin, end *signalfxformat.InputAnnotation, is *InputSpan) {
	span2 := sb.spanForEndpoint(is, begin.Endpoint)

	if is.Timestamp != nil && is.Duration != nil {
		span2.Timestamp = signalfxformat.GetPointerToInt64(is.Timestamp)
		span2.Duration = signalfxformat.GetPointerToInt64(is.Duration)
	} else {
		span2.Timestamp = signalfxformat.GetPointerToInt64(begin.Timestamp)
		if end != nil && end.Timestamp != nil && begin.Timestamp != nil {
			ts := *end.Timestamp - *begin.Timestamp
			span2.Duration = signalfxformat.GetPointerToInt64(&ts)
		}
	}
}

func (sb *spanBuilder) processBinaryAnnotations(is *InputSpan) error {
	ca, sa, ma, err := sb.pullOutSpecialBinaryAnnotations(is)
	if err != nil {
		return err
	}

	if sb.handleOnlyAddressAnnotations(is, ca, sa) {
		return nil
	}

	if sa != nil {
		sb.handleSAPresent(is, sa)
	}

	if ca != nil {
		sb.handleCAPresent(is, ca)
	}

	if ma != nil {
		sb.handleMAPresent(is, ma)
	}
	return nil
}

func (sb *spanBuilder) pullOutSpecialBinaryAnnotations(is *InputSpan) (*trace.Endpoint, *trace.Endpoint, *trace.Endpoint, error) {
	var ca, sa, ma *trace.Endpoint
	for i := range is.BinaryAnnotations {
		ba := is.BinaryAnnotations[i]
		if ba.Value == nil || ba.Key == nil {
			continue
		}
		switch val := (*ba.Value).(type) {
		case bool:
			if *ba.Key == "ca" {
				ca = ba.Endpoint
			} else if *ba.Key == "sa" {
				sa = ba.Endpoint
			} else if *ba.Key == "ma" {
				ma = ba.Endpoint
			} else {
				tagVal := "false"
				if val {
					tagVal = "true"
				}
				sb.spanForEndpoint(is, ba.Endpoint).Tags[*ba.Key] = tagVal
			}
			continue
		}

		currentSpan := sb.spanForEndpoint(is, ba.Endpoint)
		if err := sb.convertToTagOnSpan(currentSpan, ba); err != nil {
			return nil, nil, nil, err
		}
	}
	return ca, sa, ma, nil
}

func (sb *spanBuilder) convertToTagOnSpan(currentSpan *trace.Span, ba *signalfxformat.BinaryAnnotation) error {
	switch val := (*ba.Value).(type) {
	case string:
		// don't add marker "lc" tags
		if *ba.Key == "lc" && len(val) == 0 {
			return nil
		}
		currentSpan.Tags[*ba.Key] = val
	case []byte:
		currentSpan.Tags[*ba.Key] = string(val)
	case float64:
		currentSpan.Tags[*ba.Key] = strconv.FormatFloat(val, 'f', -1, 64)
	case int8, int16, int32, int64, uint8, uint16, uint32, uint64:
		currentSpan.Tags[*ba.Key] = fmt.Sprintf("%d", val)
	default:
		fmt.Printf("invalid binary annotation type of %s, for key %s for span %s\n", reflect.TypeOf(val), *ba.Key, *currentSpan.Name)
		return fmt.Errorf("invalid binary annotation type of %s, for key %s", reflect.TypeOf(val), *ba.Key)
	}
	return nil
}

// special-case when we are missing core annotations, but we have both address annotations
func (sb *spanBuilder) handleOnlyAddressAnnotations(is *InputSpan, ca, sa *trace.Endpoint) bool {
	if sb.cs == nil && sb.sr == nil && ca != nil && sa != nil {
		sb.spanForEndpoint(is, ca).RemoteEndpoint = sa
		return true
	}
	return false
}

func (sb *spanBuilder) handleSAPresent(is *InputSpan, sa *trace.Endpoint) {
	if sb.cs != nil && !closeEnough(sa, sb.cs.Endpoint) {
		sb.spanForEndpoint(is, sb.cs.Endpoint).RemoteEndpoint = sa
	} else if sb.cr != nil && !closeEnough(sa, sb.cr.Endpoint) {
		sb.spanForEndpoint(is, sb.cr.Endpoint).RemoteEndpoint = sa
	} else if sb.cs == nil && sb.cr == nil && sb.sr == nil && sb.ss == nil { // no core annotations
		s := sb.spanForEndpoint(is, nil)
		s.Kind = &ClientKind
		s.RemoteEndpoint = sa
	}
}

func (sb *spanBuilder) handleCAPresent(is *InputSpan, ca *trace.Endpoint) {
	if sb.sr != nil && !closeEnough(ca, sb.sr.Endpoint) {
		sb.spanForEndpoint(is, sb.sr.Endpoint).RemoteEndpoint = ca
	}
	if sb.ss != nil && !closeEnough(ca, sb.ss.Endpoint) {
		sb.spanForEndpoint(is, sb.ss.Endpoint).RemoteEndpoint = ca
	} else if sb.cs == nil && sb.cr == nil && sb.sr == nil && sb.ss == nil { // no core annotations
		s := sb.spanForEndpoint(is, nil)
		s.Kind = &ServerKind
		s.RemoteEndpoint = ca
	}
}

func (sb *spanBuilder) handleMAPresent(is *InputSpan, ma *trace.Endpoint) {
	if sb.ms != nil && !closeEnough(ma, sb.ms.Endpoint) {
		sb.spanForEndpoint(is, sb.ms.Endpoint).RemoteEndpoint = ma
	}
	if sb.mr != nil && !closeEnough(ma, sb.mr.Endpoint) {
		sb.spanForEndpoint(is, sb.mr.Endpoint).RemoteEndpoint = ma
	}
}

func closeEnough(left, right *trace.Endpoint) bool {
	if left.ServiceName == nil || right.ServiceName == nil {
		return left.ServiceName == nil && right.ServiceName == nil
	}
	return *left.ServiceName == *right.ServiceName
}

// An error wrapper that is nil-safe and requires no initialization.
type traceErrs struct {
	count   int
	lastErr error
}

// ToError returns the err object if the it has not been instantiated, and itself if it has
// we do this because err is possibly a response from sbingest which could be a json response
// and we want to pass this on unmolested but encoding errors are more important so send them
// if they exist
func (te *traceErrs) ToError(err error) error {
	if te == nil {
		return err
	}
	return te
}

func (te *traceErrs) Error() string {
	return fmt.Sprintf("%d errors encountered, last one was: %s", te.count, te.lastErr.Error())
}

func (te *traceErrs) Append(err error) *traceErrs {
	if err == nil {
		return te
	}

	out := te
	if out == nil {
		out = &traceErrs{}
	}
	out.count++
	out.lastErr = err

	return out
}

// A parentSpanID of all hex 0s should be normalized to nil.
func normalizeParentSpanID(parentSpanID *string) *string {
	if parentSpanID != nil && strings.Count(*parentSpanID, "0") == len(*parentSpanID) {
		return nil
	}
	return parentSpanID
}

// ParseMapOfJaegerBatchesFromRequest parses a signalfx, zipkinV1, or zipkinV2 json request into an array of jaeger batches
func ParseMapOfJaegerBatchesFromRequest(req *http.Request) (map[[32]byte]*jaegerpb.Batch, error) {
	var input signalfxformat.InputSpanList
	if err := easyjson.UnmarshalFromReader(req.Body, &input); err != nil {
		return nil, ErrInvalidJSONTraceFormat
	}

	batcher := translator.SpanBatcher{}

	// Don't let an error converting one set of spans prevent other valid spans
	// in the same request from being rejected.
	var conversionErrs *traceErrs
	for _, is := range input {
		inputSpan := (*InputSpan)(is)
		if inputSpan.isDefinitelyZipkinV2() {
			s, err := inputSpan.JaegerFromZipkinV2()
			if err != nil {
				conversionErrs = conversionErrs.Append(err)
				continue
			}
			batcher.Add(s)
		} else {
			// TODO: optimize conversion of zipkin v1 to SAPM
			derived, err := inputSpan.fromZipkinV1()
			if err != nil {
				conversionErrs = conversionErrs.Append(err)
				continue
			}

			// Zipkin v1 spans can map to multiple spans in Zipkin v2
			for _, s := range derived {
				batcher.Add(translator.SAPMSpanFromSFXSpan(s))
			}
		}
	}

	return batcher.Buckets, conversionErrs.ToError(nil)
}

// ParseSAPMFromRequest parses a signalfx, zipkinV1 or zipkinV2 json request into SAPM
func ParseSAPMFromRequest(req *http.Request) (*splunksapm.PostSpansRequest, error) {
	var sapm *splunksapm.PostSpansRequest
	batches, err := ParseMapOfJaegerBatchesFromRequest(req)
	if err == nil {
		sapm = &splunksapm.PostSpansRequest{Batches: make([]*jaegerpb.Batch, 0, len(batches))}
		for _, s := range batches {
			sapm.Batches = append(sapm.GetBatches(), s)
		}
	}
	return sapm, err
}

// JSONTraceDecoderV1 decodes json to structs
type JSONTraceDecoderV1 struct {
	Logger log.Logger
	Sink   trace.Sink
}

// ErrInvalidJSONTraceFormat is returned when we are unable to decode the request payload into []signalfxformat.InputSpan
var ErrInvalidJSONTraceFormat = errors.New("invalid JSON format; please see correct format at https://zipkin.io/zipkin-api/#/default/post_spans")

// Read the data off the wire in json format
func (decoder *JSONTraceDecoderV1) Read(ctx context.Context, req *http.Request) error {
	var input signalfxformat.InputSpanList
	if err := easyjson.UnmarshalFromReader(req.Body, &input); err != nil {
		return ErrInvalidJSONTraceFormat
	}

	if len(input) == 0 {
		return nil
	}

	spans := make([]*trace.Span, 0, len(input))

	// Don't let an error converting one set of spans prevent other valid spans
	// in the same request from being rejected.
	var conversionErrs *traceErrs
	for _, is := range input {
		inputSpan := (*InputSpan)(is)
		if inputSpan.isDefinitelyZipkinV2() {
			s, err := inputSpan.fromZipkinV2()
			is.Span.Timestamp = signalfxformat.GetPointerToInt64(inputSpan.Timestamp)
			is.Span.Duration = signalfxformat.GetPointerToInt64(inputSpan.Duration)
			if err != nil {
				conversionErrs = conversionErrs.Append(err)
				continue
			}

			spans = append(spans, s)
		} else {
			derived, err := inputSpan.fromZipkinV1()
			if err != nil {
				conversionErrs = conversionErrs.Append(err)
				continue
			}

			// Zipkin v1 spans can map to multiple spans in Zipkin v2
			spans = append(spans, derived...)
		}
	}

	err := decoder.Sink.AddSpans(ctx, spans)
	return conversionErrs.ToError(err)
}

func setupJSONTraceV1(ctx context.Context, r *mux.Router, sink Sink, logger log.Logger, httpChain web.NextConstructor, counter *dpsink.Counter) sfxclient.Collector {
	handler, st := SetupChain(ctx, sink, ZipkinV1, func(s Sink) ErrorReader {
		return &JSONTraceDecoderV1{Logger: logger, Sink: sink}
	}, httpChain, logger, counter)
	SetupJSONByPathsN(r, handler, DefaultTracePathV1, ZipkinTracePathV1, ZipkinTracePathV2)
	return st
}
