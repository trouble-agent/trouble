package sentinel

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// maxEventBytesDefault caps one assembled collector event (§3.5 guard 4).
const maxEventBytesDefault = 1024 * 1024

// goPanicCap is the §3.5 total assembly cap for go-panic dumps (1s), on top of
// its 250ms silence timeout.
const goPanicCap = 1000 * time.Millisecond

// logLine is one raw line handed by a lineSource to the assembler.
type logLine struct {
	Text   string
	TS     time.Time
	Source string // "journal:<unit>" | "file:<path>"
	Unit   string
	// Boot is the systemd monotonic/real-time marker when the source provides
	// one (journald's __REALTIME_TIMESTAMP).
	RealTS time.Time
}

// lineSource is the package-private source interface (§2.2): journalSource and
// fileTailSource implement it, so nothing new crosses a package boundary.
type lineSource interface {
	Name() string
	Lines(ctx context.Context) (<-chan logLine, <-chan error, error)
	Close() error
}

// parserDef is one entry of the §3.5 parser table.
type parserDef struct {
	Name          string
	Kind          string
	Level         string
	StartPattern  string
	Continuation  []string
	FlushTimeout  time.Duration
	MaxEventBytes int
	SigFields     []string
	Sources       []string

	start *regexp.Regexp
	cont  []*regexp.Regexp
	build func(lines []logLine) *rawEvent
}

// compile compiles the parser's patterns.
func (p *parserDef) compile() *Error {
	if p.StartPattern == "" {
		return errf(types.CodeSentinel017, "parser has no start pattern", "parser_config")
	}
	re, err := regexp.Compile(p.StartPattern)
	if err != nil {
		return errf(types.CodeSentinel017, "parser start pattern does not compile", "parser_config")
	}
	p.start = re
	p.cont = p.cont[:0]
	for _, c := range p.Continuation {
		cr, cerr := regexp.Compile(c)
		if cerr != nil {
			return errf(types.CodeSentinel017, "parser continuation pattern does not compile", "parser_config")
		}
		p.cont = append(p.cont, cr)
	}
	if p.FlushTimeout <= 0 {
		p.FlushTimeout = 250 * time.Millisecond
	}
	if p.MaxEventBytes <= 0 {
		p.MaxEventBytes = maxEventBytesDefault
	}
	return nil
}

// isStart reports whether the line starts an event for this parser.
func (p *parserDef) isStart(line string) bool { return p.start != nil && p.start.MatchString(line) }

// isContinuation reports whether the line continues the current event.
func (p *parserDef) isContinuation(line string) bool {
	for _, c := range p.cont {
		if c.MatchString(line) {
			return true
		}
	}
	return false
}

// isGoroutineMarker is the `^goroutine \d+` line that ENDs a go-panic dump.
var reGoroutineLine = regexp.MustCompile(`^goroutine \d+`)

// assembleState is the per-(source, parser) partial (§3.5 guard 3).
type assembleState struct {
	parser   *parserDef
	source   string
	lines    []logLine
	started  time.Time
	last     time.Time
	bytes    int
	trunc    bool
	nonUTF8  bool
	fromRing bool
}

// collectorSet owns the sources, the parsers and the partials.
type collectorSet struct {
	s       *Server
	parsers []*parserDef
	states  map[string]*assembleState
	sources []lineSource
	live    map[string]*sourceLive
	fed     chan logLine
}

// sourceLive is the per-source liveness expectation (§3.8's SourceLiveness).
type sourceLive struct {
	host      string
	source    string
	zone      string
	expected  bool
	alive     bool
	lastTS    string
	maxAgeS   float64
	attachErr string
}

func newCollectorSet(s *Server) *collectorSet {
	c := &collectorSet{
		s:      s,
		states: map[string]*assembleState{},
		live:   map[string]*sourceLive{},
		fed:    make(chan logLine, 1024),
	}
	c.parsers = builtinParsers()
	// Config may override a builtin parser's timeout/enablement/sources and may
	// add one of its own name (the table shape is the contract of §3.5).
	for _, cfg := range s.cfg.Collectors.Parsers {
		p := c.parserByName(cfg.Name)
		if p == nil {
			p = &parserDef{Name: cfg.Name, Kind: "multiline", Level: "error", MaxEventBytes: maxEventBytesDefault}
			c.parsers = append(c.parsers, p)
		}
		if cfg.StartPattern != "" {
			p.StartPattern = cfg.StartPattern
		}
		if len(cfg.Continuation) > 0 {
			p.Continuation = append([]string(nil), cfg.Continuation...)
		}
		if cfg.FlushTimeout.Std() > 0 {
			p.FlushTimeout = cfg.FlushTimeout.Std()
		}
		if cfg.MaxEventBytes > 0 {
			p.MaxEventBytes = cfg.MaxEventBytes
		}
		if cfg.Level != "" {
			p.Level = cfg.Level
		}
		if len(cfg.SigFields) > 0 {
			p.SigFields = append([]string(nil), cfg.SigFields...)
		}
		if len(cfg.Sources) > 0 {
			p.Sources = append([]string(nil), cfg.Sources...)
		}
		if !cfg.Enabled {
			p.Sources = nil
		}
	}
	for _, p := range c.parsers {
		if err := p.compile(); err != nil {
			s.logger.Printf("sentinel: parser %s disabled: %v", p.Name, err)
			p.Sources = nil
		}
	}
	return c
}

func (c *collectorSet) parserByName(name string) *parserDef {
	for _, p := range c.parsers {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// ParserMeta is the §3.10 health/config surface for the collectors.
func (c *collectorSet) ParserMeta() []types.CollectorParser {
	out := make([]types.CollectorParser, 0, len(c.parsers))
	for _, p := range c.parsers {
		out = append(out, types.CollectorParser{
			Name:          p.Name,
			Enabled:       len(p.Sources) > 0,
			Kind:          p.Kind,
			StartPattern:  p.StartPattern,
			Continuation:  append([]string(nil), p.Continuation...),
			FlushTimeout:  types.Duration(p.FlushTimeout.String()),
			MaxEventBytes: p.MaxEventBytes,
			Level:         p.Level,
			SigFields:     append([]string(nil), p.SigFields...),
			Sources:       append([]string(nil), p.Sources...),
		})
	}
	return out
}

// start attaches every configured source and begins consuming lines.
func (c *collectorSet) start(ctx context.Context) error {
	for _, unit := range c.s.cfg.Collectors.JournalUnits {
		src := newJournalSource(c.s, unit)
		c.sources = append(c.sources, src)
		c.register("journal:"+unit, src)
	}
	for _, path := range c.s.cfg.Collectors.FileTails {
		src := newFileTailSource(c.s, path, c.sourceParserName(path))
		c.sources = append(c.sources, src)
		c.register("file:"+path, src)
	}
	for _, src := range c.sources {
		ch, errs, err := src.Lines(ctx)
		if err != nil {
			// A source that cannot attach emits one gap and is marked dead: a
			// failure to observe is never silence (§3.5).
			c.s.markGap("collector_attach_failed", src.Name(), 1)
			c.markAttachError(src.Name(), err.Error())
			continue
		}
		c.s.wg.Add(1)
		go c.consume(ctx, src, ch, errs)
	}
	if len(c.sources) > 0 {
		c.s.wg.Add(1)
		go c.tick(ctx)
	}
	return nil
}

// sourceParserName picks the parser a file tail declares in its source list.
func (c *collectorSet) sourceParserName(path string) string {
	for _, p := range c.parsers {
		for _, src := range p.Sources {
			if src == "file:"+path {
				return p.Name
			}
		}
	}
	return ""
}

func (c *collectorSet) register(name string, src lineSource) {
	c.live[name] = &sourceLive{
		host:     c.s.cfg.HostID,
		source:   name,
		zone:     c.s.siteZone(),
		expected: true,
		alive:    true,
		maxAgeS:  2 * c.s.cfg.CanaryInterval.Std().Seconds(),
	}
}

func (c *collectorSet) markAttachError(name, msg string) {
	if l := c.live[name]; l != nil {
		l.alive = false
		l.attachErr = msg
	}
}

// consume feeds every line from a source into the dispatcher.
func (c *collectorSet) consume(ctx context.Context, src lineSource, ch <-chan logLine, errs <-chan error) {
	defer c.s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errs:
			if ok && err != nil {
				c.s.markGap("source_error", src.Name(), 1)
				c.markAttachError(src.Name(), err.Error())
			}
		case line, ok := <-ch:
			if !ok {
				return
			}
			c.feed(line)
		}
	}
}

// tick flushes partials whose silence timeout elapsed.
func (c *collectorSet) tick(ctx context.Context) {
	defer c.s.wg.Done()
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.flushExpired(c.s.now())
		}
	}
}

// feed dispatches one line to the parsers (§3.5 guards 1 and 2).
func (c *collectorSet) feed(line logLine) {
	line.Text = strings.ToValidUTF8(line.Text, "\uFFFD")
	if len(line.Text) > c.s.cfg.MaxLineBytes {
		line.Text = line.Text[:c.s.cfg.MaxLineBytes]
		c.s.counters.truncated.Add(1)
	}
	if ctx, ok := c.live[line.Source]; ok {
		ctx.lastTS = types.FormatUTC(c.s.now())
		ctx.alive = true
	}
	if line.TS.IsZero() {
		line.TS = c.s.now()
	}
	if strings.TrimSpace(line.Text) == "" {
		// A blank line is a separator inside a dump (Go puts one between the
		// panic message and the goroutine block), never a START and never an END
		// (§3.5's END rule excludes the goroutine header, which is what follows).
		// It does count as activity for the silence timeout.
		for _, p := range c.parsers {
			if st := c.states[stateKey(line.Source, p.Name)]; st != nil {
				st.last = line.TS
			}
		}
		return
	}

	// Guard 1: one line starts at most one event; parsers are tried in order and
	// the loser never sees the line.
	var starters []*parserDef
	for _, p := range c.parsers {
		if len(p.Sources) == 0 {
			continue
		}
		if p.isStart(line.Text) {
			starters = append(starters, p)
		}
	}
	if len(starters) > 1 {
		c.s.counters.parserAmbiguous.Add(uint64(len(starters) - 1))
	}
	if len(starters) > 0 {
		p := starters[0]
		key := stateKey(line.Source, p.Name)
		if st := c.states[key]; st != nil && len(st.lines) > 0 {
			if p.absorbs(st, line.Text) {
				// A py-traceback's exception line matches the START pattern but
				// is the last line of the traceback it closes: absorbing it keeps
				// one traceback in one event (the §3.5 END row's "flush
				// immediately" reading) while guard 2 still supersedes on a real
				// new traceback header.
				st.lines = append(st.lines, line)
				st.last = line.TS
				c.flushState(st, "end", false)
				return
			}
			// Guard 2: a START mid-assembly flushes the partial first.
			c.flushState(st, "superseded", true)
		}
		c.states[key] = &assembleState{
			parser: p, source: line.Source,
			lines: []logLine{line}, started: line.TS, last: line.TS,
			bytes: len(line.Text),
		}
		return
	}

	// Continuation of the newest partial for this (source, parser).
	for _, p := range c.parsers {
		if len(p.Sources) == 0 {
			continue
		}
		st := c.states[stateKey(line.Source, p.Name)]
		if st == nil || len(st.lines) == 0 {
			continue
		}
		if p.isContinuation(line.Text) {
			st.lines = append(st.lines, line)
			st.last = line.TS
			st.bytes += len(line.Text) + 1
			if st.bytes > p.MaxEventBytes {
				// Guard 4: the event size cap is a flush, not a drop.
				c.flushState(st, "size_cap", true)
			}
			return
		}
		if p.Name == "go-panic" && reGoroutineLine.MatchString(line.Text) {
			// The first goroutine block (goroutine 1) is the panicking
			// goroutine and belongs to the event; a later block means the dump
			// moved on, so the event is flushed there.
			if hasGoFileLine(st.lines) {
				c.flushState(st, "goroutine_dump", false)
				return
			}
			st.lines = append(st.lines, line)
			st.last = line.TS
			return
		}
		// First non-continuation line: END/flush for this parser. A line that
		// *is* the traceback's own final line (its exception line, which is not
		// a continuation) is absorbed first so one traceback stays one event.
		if p.absorbs(st, line.Text) {
			st.lines = append(st.lines, line)
			st.last = line.TS
		}
		c.flushState(st, "end", false)
		return
	}
}

// hasGoFileLine reports whether an assembled dump already carries a
// `file.go:NN` frame line, which is what distinguishes goroutine 1's block (part
// of the event) from a later dump block (which ends it).
func hasGoFileLine(lines []logLine) bool {
	for _, l := range lines {
		if reGoFileLine.MatchString(strings.TrimSpace(l.Text)) {
			return true
		}
	}
	return false
}

// absorbs reports whether a START line is really the final line of the open
// partial rather than the first line of a new event (py-traceback's exception
// line is both).
func (p *parserDef) absorbs(st *assembleState, line string) bool {
	if p.Name != "py-traceback" || len(st.lines) == 0 {
		return false
	}
	first := strings.TrimSpace(st.lines[0].Text)
	if !strings.HasPrefix(first, "Traceback (most recent call last)") {
		return false
	}
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "Traceback (most recent call last)") {
		return false
	}
	if rePyException.MatchString(t) {
		return true
	}
	// The wide form must still look like an exception line (a qualified name, or
	// a pinned class suffix), so an ordinary `INFO: ready` line ends the event
	// instead of joining it.
	m := rePyExceptionWide.FindStringSubmatch(t)
	if m == nil {
		return false
	}
	class := m[1]
	if strings.Contains(class, ".") {
		return true
	}
	return rePyException.MatchString(t)
}

// stateKey keys assembly state by (source, parser) — two sources can never share
// a partial (§3.5 guard 3).
func stateKey(source, parser string) string { return source + "\x1f" + parser }

// flushExpired flushes partials that went quiet past their timeout.
func (c *collectorSet) flushExpired(now time.Time) {
	for _, st := range c.states {
		if st == nil || len(st.lines) == 0 {
			continue
		}
		silence := now.Sub(st.last)
		if st.parser.Name == "go-panic" && now.Sub(st.started) >= goPanicCap {
			c.flushState(st, "timeout", true)
			continue
		}
		if silence >= st.parser.FlushTimeout {
			c.flushState(st, "timeout", true)
		}
	}
}

// flushState turns a partial into an event and clears the state.
func (c *collectorSet) flushState(st *assembleState, reason string, partial bool) {
	delete(c.states, stateKey(st.source, st.parser.Name))
	if len(st.lines) == 0 {
		return
	}
	if reason == "goroutine_dump" {
		// The dump header ends the event but adds nothing: the panic event was
		// complete.
		partial = false
	}
	ev := st.parser.build(st.lines)
	if ev == nil {
		c.s.counters.parserErrors.Add(1)
		c.s.markGap("parser_error", st.source, 1)
		return
	}
	ev.SourceKind = sourceCollector
	ev.CollectorParser = st.parser.Name
	ev.CollectorSource = st.source
	if partial {
		ev.Partial = true
		ev.FlushReason = reason
		c.s.counters.partialEvents.Add(1)
	}
	if st.trunc {
		ev.Truncated = true
	}
	if st.nonUTF8 {
		c.s.counters.nonUTF8.Add(1)
	}
	// The assembler uses the source's real timestamp when it has one, else the
	// receive time; log text timestamps never become the event TS (§3.5 guard 6).
	if ev.TS == "" {
		if !st.lines[0].RealTS.IsZero() {
			ev.TS = types.FormatUTC(st.lines[0].RealTS)
		} else {
			ev.TS = types.FormatUTC(st.lines[0].TS)
		}
	}
	if skew := c.s.now().Sub(st.lines[0].TS).Seconds(); skew > 300 || skew < -300 {
		ev.ClockSkewS = skew
	}
	entry, ok := c.s.projects.project(c.s.cfg.CanaryProject)
	if !ok {
		// Collectors are host-level: they report into the first configured
		// project (documented in docs/sentinel-compat.md §6).
		if len(c.s.cfg.Projects) == 0 {
			return
		}
		entry, _ = c.s.projects.project(c.s.cfg.Projects[0].ID)
	}
	if entry == nil {
		return
	}
	ev.AuthForm = ""
	if _, err := c.s.admitEvent(context.Background(), entry, ev, "event", "collector"); err != nil {
		if err.Code == types.CodeSentinel001 {
			// Fail-closed scrub refusal: the event is not persisted, and the
			// dropout is documented.
			c.s.markGap("scrub_refused", st.source, 1)
			return
		}
		c.s.logger.Printf("sentinel: collector event refused: %v", err)
	}
}

// liveness renders the SourceLiveness view (§2.2).
func (c *collectorSet) liveness() []types.SourceLiveness {
	out := make([]types.SourceLiveness, 0, len(c.live))
	for name, l := range c.live {
		age := -1.0
		if l.lastTS != "" {
			if ts, err := types.ParseUTC(l.lastTS); err == nil {
				age = c.s.now().Sub(ts).Seconds()
			}
		}
		alive := l.alive && l.attachErr == ""
		if alive && age >= 0 && l.maxAgeS > 0 && age > l.maxAgeS*10 {
			alive = false
		}
		out = append(out, types.SourceLiveness{
			HostID: l.host, Source: name, Zone: l.zone,
			Expected: l.expected, Alive: alive,
			LastEventTS: l.lastTS, LastEventAgeS: age, MaxAgeS: l.maxAgeS,
		})
	}
	sortLiveness(out)
	return out
}

// close stops every source.
func (c *collectorSet) close() error {
	var firstErr error
	for _, src := range c.sources {
		if err := src.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// markGap writes one `gap` record (one cause, one record).
func (s *Server) markGap(cause, scope string, estLost int) {
	_, _ = s.appendRecord(context.Background(), types.KGap, "", "sentinel",
		gapRecordPayload(cause, scope, estLost, "", types.FormatUTC(s.now())), 0)
}

// projectForCollectors returns the project collector events are attributed to.
func (s *Server) projectForCollectors() (*projectEntry, bool) {
	if s.cfg.CanaryProject != "" {
		if e, ok := s.projects.project(s.cfg.CanaryProject); ok {
			return e, true
		}
	}
	if len(s.cfg.Projects) == 0 {
		return nil, false
	}
	return s.projects.project(s.cfg.Projects[0].ID)
}

// flushSource flushes every partial belonging to a source (used on rotation,
// truncation and file removal).
func (c *collectorSet) flushSource(source, reason string) {
	for key, st := range c.states {
		if st == nil || st.source != source {
			continue
		}
		delete(c.states, key)
		if len(st.lines) == 0 {
			continue
		}
		ev := st.parser.build(st.lines)
		if ev == nil {
			continue
		}
		ev.Partial = true
		ev.FlushReason = reason
		ev.SourceKind = sourceCollector
		ev.CollectorParser = st.parser.Name
		ev.CollectorSource = source
		ev.TS = types.FormatUTC(st.lines[0].TS)
		c.s.counters.partialEvents.Add(1)
		if entry, ok := c.s.projectForCollectors(); ok {
			if _, err := c.s.admitEvent(context.Background(), entry, ev, "event", "collector"); err != nil {
				c.s.logger.Printf("sentinel: partial collector event refused: %v", err)
			}
		}
	}
}
