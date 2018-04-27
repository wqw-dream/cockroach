// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/duration"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
)

// traceTxnThreshold can be used to log SQL transactions that take
// longer than duration to complete. For example, traceTxnThreshold=1s
// will log the trace for any transaction that takes 1s or longer. To
// log traces for all transactions use traceTxnThreshold=1ns. Note
// that any positive duration will enable tracing and will slow down
// all execution because traces are gathered for all transactions even
// if they are not output.
var traceTxnThreshold = settings.RegisterDurationSetting(
	"sql.trace.txn.enable_threshold",
	"duration beyond which all transactions are traced (set to 0 to disable)", 0,
)

// traceSessionEventLogEnabled can be used to enable the event log
// that is normally kept for every SQL connection. The event log has a
// non-trivial performance impact and also reveals SQL statements
// which may be a privacy concern.
var traceSessionEventLogEnabled = settings.RegisterBoolSetting(
	"sql.trace.session_eventlog.enabled",
	"set to true to enable session tracing", false,
)

// DistSQLClusterExecMode controls the cluster default for when DistSQL is used.
var DistSQLClusterExecMode = settings.RegisterEnumSetting(
	"sql.defaults.distsql",
	"Default distributed SQL execution mode",
	"Auto",
	map[int64]string{
		int64(sessiondata.DistSQLOff):  "Off",
		int64(sessiondata.DistSQLAuto): "Auto",
		int64(sessiondata.DistSQLOn):   "On",
	},
)

// queryPhase represents a phase during a query's execution.
type queryPhase int

const (
	// The phase before start of execution (includes parsing, building a plan).
	preparing queryPhase = 0

	// Execution phase.
	executing queryPhase = 1
)

// queryMeta stores metadata about a query. Stored as reference in
// session.mu.ActiveQueries.
type queryMeta struct {
	// The timestamp when this query began execution.
	start time.Time

	// AST of the SQL statement - converted to query string only when necessary.
	stmt tree.Statement

	// States whether this query is distributed. Note that all queries,
	// including those that are distributed, have this field set to false until
	// start of execution; only at that point can we can actually determine whether
	// this query will be distributed. Use the phase variable below
	// to determine whether this query has entered execution yet.
	isDistributed bool

	// Current phase of execution of query.
	phase queryPhase

	// Cancellation function for the context associated with this query's transaction.
	ctxCancel context.CancelFunc

	// If set, this query will not be reported as part of SHOW QUERIES. This is
	// set based on the statement implementing tree.HiddenFromShowQueries.
	hidden bool
}

// cancel cancels the query associated with this queryMeta, by closing the associated
// txn context.
func (q *queryMeta) cancel() {
	q.ctxCancel()
}

// sessionDefaults mirrors fields in Session, for restoring default
// configuration values in SET ... TO DEFAULT (or RESET ...) statements.
type sessionDefaults struct {
	applicationName string
	database        string
}

// SessionArgs contains arguments for serving a client connection.
type SessionArgs struct {
	Database        string
	User            string
	ApplicationName string
	// RemoteAddr is the client's address. This is nil iff this is an internal
	// client.
	RemoteAddr net.Addr
}

// SessionRegistry stores a set of all sessions on this node.
// Use register() and deregister() to modify this registry.
type SessionRegistry struct {
	syncutil.Mutex
	store map[ClusterWideID]registrySession
}

// MakeSessionRegistry creates a new SessionRegistry with an empty set
// of sessions.
func MakeSessionRegistry() *SessionRegistry {
	return &SessionRegistry{store: make(map[ClusterWideID]registrySession)}
}

func (r *SessionRegistry) register(id ClusterWideID, s registrySession) {
	r.Lock()
	r.store[id] = s
	r.Unlock()
}

func (r *SessionRegistry) deregister(id ClusterWideID) {
	r.Lock()
	delete(r.store, id)
	r.Unlock()
}

type registrySession interface {
	user() string
	cancelQuery(queryID ClusterWideID) bool
	cancelSession()
	// serialize serializes a Session into a serverpb.Session
	// that can be served over RPC.
	serialize() serverpb.Session
}

// CancelQuery looks up the associated query in the session registry and cancels it.
func (r *SessionRegistry) CancelQuery(queryIDStr string, username string) (bool, error) {
	queryID, err := StringToClusterWideID(queryIDStr)
	if err != nil {
		return false, fmt.Errorf("query ID %s malformed: %s", queryID, err)
	}

	r.Lock()
	defer r.Unlock()

	for _, session := range r.store {
		if !(username == security.RootUser || username == session.user()) {
			// Skip this session.
			continue
		}

		if session.cancelQuery(queryID) {
			return true, nil
		}
	}

	return false, fmt.Errorf("query ID %s not found", queryID)
}

// CancelSession looks up the specified session in the session registry and cancels it.
func (r *SessionRegistry) CancelSession(sessionIDBytes []byte, username string) (bool, error) {
	sessionID := BytesToClusterWideID(sessionIDBytes)

	r.Lock()
	defer r.Unlock()

	for id, session := range r.store {
		if !(username == security.RootUser || username == session.user()) {
			// Skip this session.
			continue
		}

		if id == sessionID {
			session.cancelSession()
			return true, nil
		}
	}

	return false, fmt.Errorf("session ID %s not found", sessionID)
}

// SerializeAll returns a slice of all sessions in the registry, converted to serverpb.Sessions.
func (r *SessionRegistry) SerializeAll() []serverpb.Session {
	r.Lock()
	defer r.Unlock()

	response := make([]serverpb.Session, 0, len(r.store))

	for _, s := range r.store {
		response = append(response, s.serialize())
	}

	return response
}

func newSchemaInterface(tables *TableCollection, vt VirtualTabler) *schemaInterface {
	sc := &schemaInterface{
		physical: &CachedPhysicalAccessor{
			SchemaAccessor: UncachedPhysicalAccessor{},
			tc:             tables,
		},
	}
	sc.logical = &LogicalSchemaAccessor{
		SchemaAccessor: sc.physical,
		vt:             vt,
	}
	return sc
}

// MaxSQLBytes is the maximum length in bytes of SQL statements serialized
// into a serverpb.Session. Exported for testing.
const MaxSQLBytes = 1000

type schemaChangerCollection struct {
	schemaChangers []SchemaChanger
}

func (scc *schemaChangerCollection) queueSchemaChanger(schemaChanger SchemaChanger) {
	scc.schemaChangers = append(scc.schemaChangers, schemaChanger)
}

func (scc *schemaChangerCollection) reset() {
	scc.schemaChangers = nil
}

// execSchemaChanges releases schema leases and runs the queued
// schema changers. This needs to be run after the transaction
// scheduling the schema change has finished.
//
// The list of closures is cleared after (attempting) execution.
func (scc *schemaChangerCollection) execSchemaChanges(
	ctx context.Context, cfg *ExecutorConfig,
) error {
	if cfg.SchemaChangerTestingKnobs.SyncFilter != nil && (len(scc.schemaChangers) > 0) {
		cfg.SchemaChangerTestingKnobs.SyncFilter(TestingSchemaChangerCollection{scc})
	}
	// Execute any schema changes that were scheduled, in the order of the
	// statements that scheduled them.
	var firstError error
	for _, sc := range scc.schemaChangers {
		sc.db = cfg.DB
		sc.testingKnobs = cfg.SchemaChangerTestingKnobs
		sc.distSQLPlanner = cfg.DistSQLPlanner
		for r := retry.Start(base.DefaultRetryOptions()); r.Next(); {
			evalCtx := createSchemaChangeEvalCtx(cfg.Clock.Now())
			if err := sc.exec(ctx, true /* inSession */, &evalCtx); err != nil {
				if shouldLogSchemaChangeError(err) {
					log.Warningf(ctx, "error executing schema change: %s", err)
				}
				if err == sqlbase.ErrDescriptorNotFound {
				} else if isPermanentSchemaChangeError(err) {
					// All constraint violations can be reported; we report it as the result
					// corresponding to the statement that enqueued this changer.
					// There's some sketchiness here: we assume there's a single result
					// per statement and we clobber the result/error of the corresponding
					// statement.
					if firstError == nil {
						firstError = err
					}
				} else {
					// retryable error.
					continue
				}
			}
			break
		}
	}
	scc.schemaChangers = nil
	return firstError
}

const panicLogOutputCutoffChars = 500

func anonymizeStmtAndConstants(stmt tree.Statement) string {
	return tree.AsStringWithFlags(stmt, tree.FmtAnonymize|tree.FmtHideConstants)
}

// AnonymizeStatementsForReporting transforms an action, SQL statements, and a value
// (usually a recovered panic) into an error that will be useful when passed to
// our error reporting as it exposes a scrubbed version of the statements.
func AnonymizeStatementsForReporting(action, sqlStmts string, r interface{}) error {
	var anonymized []string
	{
		stmts, err := parser.Parse(sqlStmts)
		if err == nil {
			for _, stmt := range stmts {
				anonymized = append(anonymized, anonymizeStmtAndConstants(stmt))
			}
		}
	}
	anonStmtsStr := strings.Join(anonymized, "; ")
	if len(anonStmtsStr) > panicLogOutputCutoffChars {
		anonStmtsStr = anonStmtsStr[:panicLogOutputCutoffChars] + " [...]"
	}

	return log.Safe(
		fmt.Sprintf("panic while %s %d statements: %s", action, len(anonymized), anonStmtsStr),
	).WithCause(r)
}

// SessionTracing holds the state used by SET TRACING {ON,OFF,LOCAL} statements in
// the context of one SQL session.
// It holds the current trace being collected (or the last trace collected, if
// tracing is not currently ongoing).
//
// SessionTracing and its interactions with the connExecutor are thread-safe;
// tracing can be turned on at any time.
type SessionTracing struct {
	// enabled is set at times when "session enabled" is active - i.e. when
	// transactions are being recorded.
	enabled bool

	// kvTracingEnabled is set at times when KV tracing is active. When
	// KV tracning is enabled, the SQL/KV interface logs individual K/V
	// operators to the current context.
	kvTracingEnabled bool

	// If recording==true, recordingType indicates the type of the current
	// recording.
	recordingType tracing.RecordingType

	// ex is the connExecutor to which this SessionTracing is tied.
	ex *connExecutor

	// firstTxnSpan is the span of the first txn that was active when session
	// tracing was enabled.
	firstTxnSpan opentracing.Span

	// connSpan is the connection's span. This is recording.
	connSpan opentracing.Span

	// lastRecording will collect the recording when stopping tracing.
	lastRecording []traceRow
}

// getRecording returns the session trace. If we're not currently tracing, this
// will be the last recorded trace. If we are currently tracing, we'll return
// whatever was recorded so far.
func (st *SessionTracing) getRecording() ([]traceRow, error) {
	if !st.enabled {
		return st.lastRecording, nil
	}

	var spans []tracing.RecordedSpan
	if st.firstTxnSpan != nil {
		spans = append(spans, tracing.GetRecording(st.firstTxnSpan)...)
	}
	spans = append(spans, tracing.GetRecording(st.connSpan)...)

	return generateSessionTraceVTable(spans)
}

// StartTracing starts "session tracing". From this moment on, everything
// happening on both the connection's context and the current txn's context (if
// any) will be traced.
// StopTracing() needs to be called to finish this trace.
//
// There's two contexts on which we must record:
// 1) If we're inside a txn, we start recording on the txn's span. We assume
// that the txn's ctx has a recordable span on it.
// 2) Regardless of whether we're in a txn or not, we need to record the
// connection's context. This context generally does not have a span, so we
// "hijack" it with one that does. Whatever happens on that context, plus
// whatever happens in future derived txn contexts, will be recorded.
//
// Args:
// kvTracingEnabled: If set, the traces will also include "KV trace" messages -
//   verbose messages around the interaction of SQL with KV. Some of the messages
//   are per-row.
func (st *SessionTracing) StartTracing(recType tracing.RecordingType, kvTracingEnabled bool) error {
	if st.enabled {
		// We're already tracing. No-op.
		return nil
	}

	// If we're inside a transaction, start recording on the txn span.
	if _, ok := st.ex.machine.CurState().(stateNoTxn); !ok {
		sp := opentracing.SpanFromContext(st.ex.state.Ctx)
		if sp == nil {
			return errors.Errorf("no txn span for SessionTracing")
		}
		tracing.StartRecording(sp, recType)
		st.firstTxnSpan = sp
	}

	st.enabled = true
	st.kvTracingEnabled = kvTracingEnabled
	st.recordingType = recType

	// Now hijack the conn's ctx with one that has a recording span.

	opName := "session recording"
	var sp opentracing.Span
	if parentSp := opentracing.SpanFromContext(st.ex.ctxHolder.connCtx); parentSp != nil {
		// Create a child span while recording.
		sp = parentSp.Tracer().StartSpan(
			opName, opentracing.ChildOf(parentSp.Context()), tracing.Recordable)
	} else {
		// Create a root span while recording.
		sp = st.ex.server.cfg.AmbientCtx.Tracer.StartSpan(opName, tracing.Recordable)
	}
	tracing.StartRecording(sp, recType)
	st.connSpan = sp

	// Hijack the connections context.
	newConnCtx := opentracing.ContextWithSpan(st.ex.ctxHolder.connCtx, sp)
	st.ex.ctxHolder.hijack(newConnCtx)

	return nil
}

// StopTracing stops the trace that was started with StartTracing().
// An error is returned if tracing was not active.
func (st *SessionTracing) StopTracing() error {
	if !st.enabled {
		// We're not currently tracing. No-op.
		return nil
	}
	st.enabled = false

	var spans []tracing.RecordedSpan

	if st.firstTxnSpan != nil {
		spans = append(spans, tracing.GetRecording(st.firstTxnSpan)...)
		tracing.StopRecording(st.firstTxnSpan)
	}
	st.connSpan.Finish()
	spans = append(spans, tracing.GetRecording(st.connSpan)...)
	// NOTE: We're stopping recording on the connection's ctx only; the stopping
	// is not inherited by children. If we are inside of a txn, that span will
	// continue recording, even though nobody will collect its recording again.
	tracing.StopRecording(st.connSpan)
	st.ex.ctxHolder.unhijack()

	var err error
	st.lastRecording, err = generateSessionTraceVTable(spans)
	return err
}

// RecordingType returns which type of tracing is currently being done.
func (st *SessionTracing) RecordingType() tracing.RecordingType {
	return st.recordingType
}

// KVTracingEnabled checks whether KV tracing is currently enabled.
func (st *SessionTracing) KVTracingEnabled() bool {
	return st.kvTracingEnabled
}

// Enabled checks whether session tracing is currently enabled.
func (st *SessionTracing) Enabled() bool {
	return st.enabled
}

// extractMsgFromRecord extracts the message of the event, which is either in an
// "event" or "error" field.
func extractMsgFromRecord(rec tracing.RecordedSpan_LogRecord) string {
	for _, f := range rec.Fields {
		key := f.Key
		if key == "event" {
			return f.Value
		}
		if key == "error" {
			return fmt.Sprint("error:", f.Value)
		}
	}
	return "<event missing in trace message>"
}

// traceRow is the type of a single row in the session_trace vtable.
// The columns are as follows:
// - span_idx
// - message_idx
// - timestamp
// - duration
// - operation
// - location
// - tag
// - message
type traceRow [8]tree.Datum

// A regular expression to split log messages.
// It has three parts:
// - the (optional) code location, with at least one forward slash and a period
//   in the file name:
//   ((?:[^][ :]+/[^][ :]+\.[^][ :]+:[0-9]+)?)
// - the (optional) tag: ((?:\[(?:[^][]|\[[^]]*\])*\])?)
// - the message itself: the rest.
var logMessageRE = regexp.MustCompile(
	`(?s:^((?:[^][ :]+/[^][ :]+\.[^][ :]+:[0-9]+)?) *((?:\[(?:[^][]|\[[^]]*\])*\])?) *(.*))`)

// generateSessionTraceVTable generates the rows of said table by using the log
// messages from the session's trace (i.e. the ongoing trace, if any, or the
// last one recorded).
//
// All the log messages from the current recording are returned, in
// the order in which they should be presented in the crdb_internal.session_info
// virtual table. Messages from child spans are inserted as a block in between
// messages from the parent span. Messages from sibling spans are not
// interleaved.
//
// Here's a drawing showing the order in which messages from different spans
// will be interleaved. Each box is a span; inner-boxes are child spans. The
// numbers indicate the order in which the log messages will appear in the
// virtual table.
//
// +-----------------------+
// |           1           |
// | +-------------------+ |
// | |         2         | |
// | |  +----+           | |
// | |  |    | +----+    | |
// | |  | 3  | | 4  |    | |
// | |  |    | |    |  5 | |
// | |  |    | |    | ++ | |
// | |  |    | |    |    | |
// | |  +----+ |    |    | |
// | |         +----+    | |
// | |                   | |
// | |          6        | |
// | +-------------------+ |
// |            7          |
// +-----------------------+
//
// Note that what's described above is not the order in which SHOW TRACE FOR ...
// displays the information.
func generateSessionTraceVTable(spans []tracing.RecordedSpan) ([]traceRow, error) {
	// Get all the log messages, in the right order.
	var allLogs []logRecordRow

	// NOTE: The spans are recorded in the order in which they are started.
	seenSpans := make(map[uint64]struct{})
	for spanIdx, span := range spans {
		if _, ok := seenSpans[span.SpanID]; ok {
			continue
		}
		spanWithIndex := spanWithIndex{
			RecordedSpan: &spans[spanIdx],
			index:        spanIdx,
		}
		msgs, err := getMessagesForSubtrace(spanWithIndex, spans, seenSpans)
		if err != nil {
			return nil, err
		}
		allLogs = append(allLogs, msgs...)
	}

	// Transform the log messages into table rows.
	var res []traceRow
	for _, lrr := range allLogs {
		// The "operation" column is only set for the first row in span.
		var operation tree.Datum
		if lrr.index == 0 {
			operation = tree.NewDString(lrr.span.Operation)
		} else {
			operation = tree.DNull
		}
		var dur tree.Datum
		if lrr.index == 0 && lrr.span.Duration != 0 {
			dur = &tree.DInterval{
				Duration: duration.Duration{
					Nanos: lrr.span.Duration.Nanoseconds(),
				},
			}
		} else {
			// Span was not finished.
			dur = tree.DNull
		}

		// Split the message into component parts.
		//
		// The result of FindStringSubmatchIndex is a 1D array of pairs
		// [start, end) of positions in the input string.  The first pair
		// identifies the entire match; the 2nd pair corresponds to the
		// 1st parenthetized expression in the regexp, and so on.
		loc := logMessageRE.FindStringSubmatchIndex(lrr.msg)
		if loc == nil {
			return nil, fmt.Errorf("unable to split trace message: %q", lrr.msg)
		}

		row := traceRow{
			tree.NewDInt(tree.DInt(lrr.span.index)),               // span_idx
			tree.NewDInt(tree.DInt(lrr.index)),                    // message_idx
			tree.MakeDTimestampTZ(lrr.timestamp, time.Nanosecond), // timestamp
			dur,       // duration
			operation, // operation
			tree.NewDString(lrr.msg[loc[2]:loc[3]]), // location
			tree.NewDString(lrr.msg[loc[4]:loc[5]]), // tag
			tree.NewDString(lrr.msg[loc[6]:loc[7]]), // message
		}
		res = append(res, row)
	}
	return res, nil
}

// getOrderedChildSpans returns all the spans in allSpans that are children of
// spanID. It assumes the input is ordered by start time, in which case the
// output is also ordered.
func getOrderedChildSpans(spanID uint64, allSpans []tracing.RecordedSpan) []spanWithIndex {
	children := make([]spanWithIndex, 0)
	for i := range allSpans {
		if allSpans[i].ParentSpanID == spanID {
			children = append(
				children,
				spanWithIndex{
					RecordedSpan: &allSpans[i],
					index:        i,
				})
		}
	}
	return children
}

// getMessagesForSubtrace takes a span and interleaves its log messages with
// those from its children (recursively). The order is the one defined in the
// comment on generateSessionTraceVTable().
//
// seenSpans is modified to record all the spans that are part of the subtrace
// rooted at span.
func getMessagesForSubtrace(
	span spanWithIndex, allSpans []tracing.RecordedSpan, seenSpans map[uint64]struct{},
) ([]logRecordRow, error) {
	if _, ok := seenSpans[span.SpanID]; ok {
		return nil, errors.Errorf("duplicate span %d", span.SpanID)
	}
	var allLogs []logRecordRow
	const spanStartMsgTemplate = "=== SPAN START: %s ==="

	// Add a dummy log message marking the beginning of the span, to indicate
	// the start time and duration of span.
	allLogs = append(allLogs,
		logRecordRow{
			timestamp: span.StartTime,
			msg:       fmt.Sprintf(spanStartMsgTemplate, span.Operation),
			span:      span,
			index:     0,
		})

	seenSpans[span.SpanID] = struct{}{}
	childSpans := getOrderedChildSpans(span.SpanID, allSpans)
	var i, j int
	// Sentinel value - year 6000.
	maxTime := time.Date(6000, 0, 0, 0, 0, 0, 0, time.UTC)
	// Merge the logs with the child spans.
	for i < len(span.Logs) || j < len(childSpans) {
		logTime := maxTime
		childTime := maxTime
		if i < len(span.Logs) {
			logTime = span.Logs[i].Time
		}
		if j < len(childSpans) {
			childTime = childSpans[j].StartTime
		}

		if logTime.Before(childTime) {
			allLogs = append(allLogs,
				logRecordRow{
					timestamp: logTime,
					msg:       extractMsgFromRecord(span.Logs[i]),
					span:      span,
					// Add 1 to the index to account for the first dummy message in a span.
					index: i + 1,
				})
			i++
		} else {
			// Recursively append messages from the trace rooted at the child.
			childMsgs, err := getMessagesForSubtrace(childSpans[j], allSpans, seenSpans)
			if err != nil {
				return nil, err
			}
			allLogs = append(allLogs, childMsgs...)
			j++
		}
	}
	return allLogs, nil
}

// logRecordRow is used to temporarily hold on to log messages and their
// metadata while flattening a trace.
type logRecordRow struct {
	timestamp time.Time
	msg       string
	span      spanWithIndex
	// index of the log message within its span.
	index int
}

type spanWithIndex struct {
	*tracing.RecordedSpan
	index int
}

// sessionDataMutator is the interface used by sessionVars to change the session
// state. It mostly mutates the Session's SessionData, but not exclusively (e.g.
// see curTxnReadOnly).
type sessionDataMutator struct {
	data     *sessiondata.SessionData
	defaults sessionDefaults
	settings *cluster.Settings
	// curTxnReadOnly is a value to be mutated through SET transaction_read_only = ...
	curTxnReadOnly *bool
	// applicationNamedChanged, if set, is called when the "application name"
	// variable is updated.
	applicationNameChanged func(newName string)
}

// SetApplicationName sets the application name.
func (m *sessionDataMutator) SetApplicationName(appName string) {
	m.data.ApplicationName = appName
	if m.applicationNameChanged != nil {
		m.applicationNameChanged(appName)
	}
}

func (m *sessionDataMutator) SetDatabase(dbName string) {
	m.data.Database = dbName
}

func (m *sessionDataMutator) SetDefaultIsolationLevel(iso enginepb.IsolationType) {
	m.data.DefaultIsolationLevel = iso
}

func (m *sessionDataMutator) SetDefaultReadOnly(val bool) {
	m.data.DefaultReadOnly = val
}

func (m *sessionDataMutator) SetDistSQLMode(val sessiondata.DistSQLExecMode) {
	m.data.DistSQLMode = val
}

func (m *sessionDataMutator) SetLookupJoinEnabled(val bool) {
	m.data.LookupJoinEnabled = val
}

func (m *sessionDataMutator) SetZigzagJoinEnabled(val bool) {
	m.data.ZigzagJoinEnabled = val
}

func (m *sessionDataMutator) SetOptimizerMode(val sessiondata.OptimizerMode) {
	m.data.OptimizerMode = val
}

func (m *sessionDataMutator) SetSafeUpdates(val bool) {
	m.data.SafeUpdates = val
}

func (m *sessionDataMutator) SetSearchPath(val sessiondata.SearchPath) {
	m.data.SearchPath = val
}

func (m *sessionDataMutator) SetLocation(loc *time.Location) {
	m.data.Location = loc
}

func (m *sessionDataMutator) SetReadOnly(val bool) {
	*m.curTxnReadOnly = val
}

func (m *sessionDataMutator) SetStmtTimeout(timeout time.Duration) {
	m.data.StmtTimeout = timeout
}

// RecordLatestSequenceValue records that value to which the session incremented
// a sequence.
func (m *sessionDataMutator) RecordLatestSequenceVal(seqID uint32, val int64) {
	m.data.SequenceState.RecordValue(seqID, val)
}

type sqlStatsCollectorImpl struct {
	// sqlStats tracks per-application statistics for all
	// applications on each node.
	sqlStats *sqlStats
	// appStats track per-application SQL usage statistics. This is a pointer into
	// sqlStats set as the session's current app.
	appStats *appStats
	// phaseTimes tracks session-level phase times. It is copied-by-value
	// to each planner in session.newPlanner.
	phaseTimes phaseTimes
}

// sqlStatsCollectorImpl implements the sqlStatsCollector interface.
var _ sqlStatsCollector = &sqlStatsCollectorImpl{}

// newSQLStatsCollectorImpl creates an instance of sqlStatsCollectorImpl.
//
// note that phaseTimes is an array, not a slice, so this performs a copy-by-value.
func newSQLStatsCollectorImpl(
	sqlStats *sqlStats, appStats *appStats, phaseTimes phaseTimes,
) *sqlStatsCollectorImpl {
	return &sqlStatsCollectorImpl{
		sqlStats:   sqlStats,
		appStats:   appStats,
		phaseTimes: phaseTimes,
	}
}

// PhaseTimes is part of the sqlStatsCollector interface.
func (s *sqlStatsCollectorImpl) PhaseTimes() *phaseTimes {
	return &s.phaseTimes
}

// RecordStatement is part of the sqlStatsCollector interface.
func (s *sqlStatsCollectorImpl) RecordStatement(
	stmt Statement,
	distSQLUsed bool,
	automaticRetryCount int,
	numRows int,
	err error,
	parseLat, planLat, runLat, svcLat, ovhLat float64,
) {
	s.appStats.recordStatement(
		stmt, distSQLUsed, automaticRetryCount, numRows, err,
		parseLat, planLat, runLat, svcLat, ovhLat)
}

// SQLStats is part of the sqlStatsCollector interface.
func (s *sqlStatsCollectorImpl) SQLStats() *sqlStats {
	return s.sqlStats
}
