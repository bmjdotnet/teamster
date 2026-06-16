// Command feed renders the live Teamster event stream.
//
// Default (TTY, no flags): header pinned at top, newest events appended at bottom.
// --dashboard: header pushing entries down, scrollback via PgUp/PgDn/End/Home.
// --tail: pure streaming mode — no header, no scrollback, safe to pipe.
// Non-TTY stdout: automatically uses pure-tail mode with no color.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/render"
)

// Region describes the rectangular draw area for the feed pane.
type Region struct {
	Width, Height        int
	OriginRow, OriginCol int
}

func (r Region) bodyHeight() int {
	if r.Height <= 1 {
		return 0
	}
	return r.Height - 1
}

const bufCap = 5000

// bufEntry pairs the source record with the teamMap snapshot at event time.
type bufEntry struct {
	rec       render.Record
	teamSnap  map[string]string
	lineCount int
}

// newEventMsg is delivered by the tail goroutine to the Bubble Tea program.
type newEventMsg render.Record

// model is the Bubble Tea application state.
type model struct {
	region        Region
	buf           []bufEntry
	viewOffset    int
	teamMap       map[string]string
	excludeTags   map[string]bool
	sessionFilter string
	events        <-chan render.Record
	agentWidth    int
	sessionWidth  int
	dashboardMode bool // --dashboard: newest at top, header pushes entries down
	colorize      bool // false when --no-color is set
}

// startTail launches a background goroutine that reads the JSONL tail and sends
// displayable records to the returned channel.
func startTail(f *os.File, startOffset int64, sessionFilter string) <-chan render.Record {
	ch := make(chan render.Record, 16)
	go func() {
		offset := startOffset
		for {
			line, err := readNextLine(f, &offset)
			if err != nil || line == "" {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			var r render.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				continue
			}
			if sessionFilter != "" && !strings.HasPrefix(r.Session, sessionFilter) {
				continue
			}
			ch <- r
		}
	}()
	return ch
}

// waitCmd returns a Cmd that blocks until the next record arrives.
func (m model) waitCmd() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		return newEventMsg(<-ch)
	}
}

// readNextLine reads one newline-terminated line from f starting at *offset.
func readNextLine(f *os.File, offset *int64) (string, error) {
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 1)
	for {
		n, err := f.ReadAt(tmp, *offset)
		if n == 0 {
			return "", err
		}
		*offset++
		if tmp[0] == '\n' {
			return strings.TrimSpace(string(buf)), nil
		}
		buf = append(buf, tmp[0])
	}
}

// entryLines returns the number of rendered terminal lines for e.
func (m *model) entryLines(e bufEntry) int {
	teamFor := func(sid string) string { return e.teamSnap[sid] }
	n := len(render.FormatLine(e.rec, m.excludeTags, m.agentWidth, m.sessionWidth, teamFor))
	if n < 1 {
		n = 1
	}
	return n
}

func totalLines(buf []bufEntry) int {
	total := 0
	for _, e := range buf {
		total += e.lineCount
	}
	return total
}

func (m *model) maxOffset() int {
	tl := totalLines(m.buf)
	bh := m.region.bodyHeight()
	max := tl - bh
	if max < 0 {
		max = 0
	}
	return max
}

func newModel(logPath string, excludeTags map[string]bool, sessionFilter string, historyN int) (*model, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}

	m := &model{
		teamMap:       make(map[string]string),
		excludeTags:   excludeTags,
		sessionFilter: sessionFilter,
		agentWidth:    5,
		sessionWidth:  render.SessionColMin,
	}
	if hl := len(render.SessionHeaderLabel); m.sessionWidth < hl {
		m.sessionWidth = hl
	}

	if historyN > 0 {
		m.loadHistory(f, historyN)
	}

	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("seek: %w", err)
	}

	m.events = startTail(f, off, sessionFilter)
	return m, nil
}

func (m *model) loadHistory(f *os.File, n int) {
	records, err := collectHistory(f, n, m.sessionFilter)
	if err != nil {
		return
	}

	wantSessions := make(map[string]bool, len(records))
	for _, r := range records {
		wantSessions[r.Session] = true
	}
	backscanned := findTeamBindings(f, wantSessions, 10000)

	localTeam := make(map[string]string)
	for sid, team := range backscanned {
		if team != "" {
			localTeam[sid] = team
		}
	}
	type snap struct{ team map[string]string }
	snaps := make([]snap, len(records))

	for i, r := range records {
		if r.Team != "" {
			localTeam[r.Session] = r.Team
		} else if r.Tool == "TeamDelete" {
			delete(localTeam, r.Session)
		}
		cp := make(map[string]string, len(localTeam))
		for k, v := range localTeam {
			cp[k] = v
		}
		snaps[i] = snap{team: cp}

		agent := r.AgentName
		if agent == "" {
			agent = "@lead"
		}
		if len(agent) > m.agentWidth {
			m.agentWidth = len(agent)
		}
		label, _ := render.SessionLabel(r, func(sid string) string { return cp[sid] })
		if lw := len(label); lw > m.sessionWidth && lw <= render.SessionColMax {
			m.sessionWidth = lw
		}
	}

	for k, v := range localTeam {
		m.teamMap[k] = v
	}

	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		snap := snaps[i].team
		if !render.IsDisplayable(r) {
			continue
		}
		e := bufEntry{rec: r, teamSnap: snap}
		e.lineCount = m.entryLines(e)
		m.buf = append(m.buf, e)
	}
	m.trimBuf()
}

func (m model) Init() tea.Cmd {
	return m.waitCmd()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyPgUp:
			bh := m.region.bodyHeight()
			step := bh - 1
			if step < 1 {
				step = 1
			}
			m.viewOffset += step
			if max := m.maxOffset(); m.viewOffset > max {
				m.viewOffset = max
			}
		case tea.KeyPgDown:
			bh := m.region.bodyHeight()
			step := bh - 1
			if step < 1 {
				step = 1
			}
			m.viewOffset -= step
			if m.viewOffset < 0 {
				m.viewOffset = 0
			}
		case tea.KeyEnd:
			m.viewOffset = 0
		case tea.KeyHome:
			m.viewOffset = m.maxOffset()
		}

	case tea.WindowSizeMsg:
		m.region = Region{Width: msg.Width, Height: msg.Height}
		if max := m.maxOffset(); m.viewOffset > max {
			m.viewOffset = max
		}
		// Must re-subscribe: returning nil here drops the event channel permanently.
		return m, m.waitCmd()

	case newEventMsg:
		r := render.Record(msg)

		if r.Team != "" {
			m.teamMap[r.Session] = r.Team
		} else if r.Tool == "TeamDelete" {
			delete(m.teamMap, r.Session)
		}

		agent := r.AgentName
		if agent == "" {
			agent = "@lead"
		}
		teamFor := func(sid string) string { return m.teamMap[sid] }
		label, _ := render.SessionLabel(r, teamFor)
		if len(agent) > m.agentWidth {
			m.agentWidth = len(agent)
		}
		if lw := len(label); lw > m.sessionWidth && lw <= render.SessionColMax {
			m.sessionWidth = lw
		}

		snap := make(map[string]string, len(m.teamMap))
		for k, v := range m.teamMap {
			snap[k] = v
		}

		if render.IsDisplayable(r) {
			e := bufEntry{rec: r, teamSnap: snap}
			e.lineCount = m.entryLines(e)
			m.buf = append([]bufEntry{e}, m.buf...)
			if m.viewOffset > 0 {
				m.viewOffset += e.lineCount
				if max := m.maxOffset(); m.viewOffset > max {
					m.viewOffset = max
				}
			}
			evicted := m.trimBuf()
			if evicted {
				m.recomputeWidthsFromBuf()
			}
		}

		return m, m.waitCmd()
	}

	return m, nil
}

func (m *model) trimBuf() bool {
	if len(m.buf) > bufCap {
		m.buf = m.buf[:bufCap]
		return true
	}
	return false
}

func (m *model) recomputeWidthsFromBuf() {
	aw := 5
	sw := render.SessionColMin
	if hl := len(render.SessionHeaderLabel); sw < hl {
		sw = hl
	}
	for _, e := range m.buf {
		agent := e.rec.AgentName
		if agent == "" {
			agent = "@lead"
		}
		if len(agent) > aw {
			aw = len(agent)
		}
		label, _ := render.SessionLabel(e.rec, func(sid string) string { return e.teamSnap[sid] })
		if lw := len(label); lw > sw && lw <= render.SessionColMax {
			sw = lw
		}
	}
	m.agentWidth = aw
	m.sessionWidth = sw
}

func (m model) View() string {
	return m.viewRegion(m.region)
}

func headerStyle(width int, scrolled bool) lipgloss.Style {
	bg := lipgloss.Color("4")
	if scrolled {
		bg = lipgloss.Color("240")
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("15")).
		Background(bg).
		Bold(true).
		Width(width)
}

const scrollMarker = "  ◂ scrolled (End)"

func (m model) viewport(bh int) []bufEntry {
	if len(m.buf) == 0 || bh <= 0 {
		return nil
	}
	skip := m.viewOffset
	start := 0
	for start < len(m.buf) && skip > 0 {
		lc := m.buf[start].lineCount
		if lc <= skip {
			skip -= lc
			start++
		} else {
			break
		}
	}
	collected := 0
	end := start
	for end < len(m.buf) && collected < bh {
		collected += m.buf[end].lineCount
		end++
	}
	if start >= end {
		return nil
	}
	return m.buf[start:end]
}

// viewRegion renders the TUI pane (used in both dashboard and default tail modes).
// dashboardMode=true: newest at top (row 1), older push down.
// dashboardMode=false: newest at bottom, blank padding at top.
func (m model) viewRegion(r Region) string {
	if r.Width == 0 || r.Height == 0 {
		return ""
	}

	colorLine := func(l string) string {
		if !m.colorize {
			return display.StripANSI(l)
		}
		return l
	}

	var sb strings.Builder
	scrolled := m.viewOffset > 0

	hdrPlain := render.FormatHeaderPlain(m.sessionWidth, m.agentWidth)
	hdrText := hdrPlain
	if scrolled && len(hdrPlain)+len(scrollMarker) <= r.Width {
		hdrText = hdrPlain + scrollMarker
	}
	if m.colorize {
		sb.WriteString(headerStyle(r.Width, scrolled).Render(hdrText))
	} else {
		sb.WriteString(hdrText)
	}

	bh := r.bodyHeight()
	vp := m.viewport(bh)

	if m.dashboardMode {
		// Dashboard: newest first. vp[0] is newest; emit up to bodyHeight rendered lines.
		remaining := bh
		for _, e := range vp {
			if remaining <= 0 {
				break
			}
			teamFor := func(sid string) string { return e.teamSnap[sid] }
			lines := render.FormatLine(e.rec, m.excludeTags, m.agentWidth, m.sessionWidth, teamFor)
			for _, l := range lines {
				if remaining <= 0 {
					break
				}
				sb.WriteByte('\n')
				sb.WriteString(display.TruncateLine(colorLine(l), r.Width))
				remaining--
			}
		}
		return sb.String()
	}

	// Default tail mode: collect oldest-first (reverse vp), anchor newest at bottom.
	var bodyLines []string
	for i := len(vp) - 1; i >= 0; i-- {
		e := vp[i]
		teamFor := func(sid string) string { return e.teamSnap[sid] }
		lines := render.FormatLine(e.rec, m.excludeTags, m.agentWidth, m.sessionWidth, teamFor)
		bodyLines = append(bodyLines, lines...)
	}
	if len(bodyLines) > bh {
		if scrolled {
			bodyLines = bodyLines[:bh]
		} else {
			bodyLines = bodyLines[len(bodyLines)-bh:]
		}
	}
	blanks := bh - len(bodyLines)
	for i := 0; i < blanks; i++ {
		sb.WriteByte('\n')
	}
	for _, l := range bodyLines {
		sb.WriteByte('\n')
		sb.WriteString(display.TruncateLine(colorLine(l), r.Width))
	}

	return sb.String()
}

// findTeamBindings scans backward up to scanLimit lines to find the most-recent
// TeamCreate/TeamDelete binding for each session in wantSessions.
func findTeamBindings(f *os.File, wantSessions map[string]bool, scanLimit int) map[string]string {
	const chunkSize = 64 * 1024
	result := make(map[string]string)
	found := make(map[string]bool)

	info, err := f.Stat()
	if err != nil {
		return result
	}
	fileSize := info.Size()
	offset := fileSize
	var tail []byte
	scanned := 0

	for offset > 0 && scanned < scanLimit && len(found) < len(wantSessions) {
		readSize := int64(chunkSize)
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize

		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, offset); err != nil {
			break
		}

		chunk := append(buf, tail...)
		lines := strings.Split(string(chunk), "\n")

		if offset > 0 {
			tail = []byte(lines[0])
			lines = lines[1:]
		} else {
			tail = nil
		}

		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			scanned++
			var r render.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				continue
			}
			if !wantSessions[r.Session] || found[r.Session] {
				continue
			}
			if r.Team != "" {
				result[r.Session] = r.Team
				found[r.Session] = true
			} else if r.Tool == "TeamDelete" {
				result[r.Session] = ""
				found[r.Session] = true
			}
			if scanned >= scanLimit || len(found) >= len(wantSessions) {
				break
			}
		}
	}
	return result
}

// collectHistory reads the last n displayable records in chronological order.
func collectHistory(f *os.File, n int, sessionFilter string) ([]render.Record, error) {
	const chunkSize = 64 * 1024

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := info.Size()

	var collected []render.Record
	offset := fileSize
	var tail []byte

	for offset > 0 && len(collected) < n {
		readSize := int64(chunkSize)
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize

		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, offset); err != nil {
			return nil, err
		}

		chunk := append(buf, tail...)
		lines := strings.Split(string(chunk), "\n")

		if offset > 0 {
			tail = []byte(lines[0])
			lines = lines[1:]
		} else {
			tail = nil
		}

		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var r render.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				continue
			}
			if sessionFilter != "" && !strings.HasPrefix(r.Session, sessionFilter) {
				continue
			}
			if !render.IsDisplayable(r) {
				continue
			}
			collected = append(collected, r)
			if len(collected) >= n {
				break
			}
		}
	}

	if len(collected) < n && len(tail) > 0 {
		line := strings.TrimSpace(string(tail))
		if line != "" {
			var r render.Record
			if err := json.Unmarshal([]byte(line), &r); err == nil {
				if sessionFilter == "" || strings.HasPrefix(r.Session, sessionFilter) {
					if render.IsDisplayable(r) {
						collected = append(collected, r)
					}
				}
			}
		}
	}

	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}

// runPureTail streams events to stdout with no header, no alt-screen, no scrollback.
// Safe to pipe into less, grep, or files.
// backfillN lines of history are printed before following live appends.
func runPureTail(logPath string, excludeTags map[string]bool, sessionFilter string, colorize bool, backfillN int) error {
	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer f.Close()

	teamMap := map[string]string{}
	agentWidth := 5
	sessionWidth := render.SessionColMin
	if hl := len(render.SessionHeaderLabel); sessionWidth < hl {
		sessionWidth = hl
	}

	w := bufio.NewWriter(os.Stdout)
	teamFor := func(sid string) string { return teamMap[sid] }

	// Backfill last N records before following.
	if backfillN > 0 {
		records, _ := collectHistory(f, backfillN, sessionFilter)
		for _, r := range records {
			if r.Team != "" {
				teamMap[r.Session] = r.Team
			} else if r.Tool == "TeamDelete" {
				delete(teamMap, r.Session)
			}
			agent := r.AgentName
			if agent == "" {
				agent = "@lead"
			}
			if len(agent) > agentWidth {
				agentWidth = len(agent)
			}
			label, _ := render.SessionLabel(r, teamFor)
			if lw := len(label); lw > sessionWidth && lw <= render.SessionColMax {
				sessionWidth = lw
			}
			lines := render.FormatLine(r, excludeTags, agentWidth, sessionWidth, teamFor)
			for _, l := range lines {
				if !colorize {
					l = display.StripANSI(l)
				}
				fmt.Fprintln(w, l)
			}
		}
		w.Flush()
	}

	// Seek to end before following live appends.
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	// Follow using ReadAt so EOF never gets "stuck" in a scanner buffer.
	// bufio.Scanner marks EOF as terminal; recreating it each tick still reads
	// from the current file position which never advances past the last EOF.
	// ReadAt + manual offset is the correct follow pattern.
	for {
		line, err := readNextLine(f, &offset)
		if err != nil || line == "" {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var r render.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if sessionFilter != "" && !strings.HasPrefix(r.Session, sessionFilter) {
			continue
		}
		if r.Team != "" {
			teamMap[r.Session] = r.Team
		} else if r.Tool == "TeamDelete" {
			delete(teamMap, r.Session)
		}
		agent := r.AgentName
		if agent == "" {
			agent = "@lead"
		}
		if len(agent) > agentWidth {
			agentWidth = len(agent)
		}
		label, _ := render.SessionLabel(r, teamFor)
		if lw := len(label); lw > sessionWidth && lw <= render.SessionColMax {
			sessionWidth = lw
		}
		lines := render.FormatLine(r, excludeTags, agentWidth, sessionWidth, teamFor)
		for _, l := range lines {
			if !colorize {
				l = display.StripANSI(l)
			}
			fmt.Fprintln(w, l)
		}
		w.Flush()
	}
}

func main() {
	basedir := flag.String("basedir", "", "")
	excludeStr := flag.String("exclude", "", "")
	sessionFilter := flag.String("session", "", "")
	historyN := flag.Int("history", 0, "")
	flagTail := flag.Bool("tail", false, "")
	flagDashboard := flag.Bool("dashboard", false, "")
	flagNoColor := flag.Bool("no-color", false, "")
	flagLines := flag.Int("lines", 10, "")
	flag.StringVar(excludeStr, "x", "", "")
	flag.StringVar(sessionFilter, "s", "", "")
	flag.IntVar(flagLines, "n", 10, "")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: feed [flags] [logfile]\n\nFlags:\n")
		fmt.Fprintf(os.Stderr, "  --basedir DIR        Teamster base directory (overrides TEAMSTER_BASEDIR)\n")
		fmt.Fprintf(os.Stderr, "  --tail               force pure-tail mode: stream lines with no header, no scrollback (pipe-safe)\n")
		fmt.Fprintf(os.Stderr, "  --dashboard          force dashboard mode: header on top, newest pushes down, scrollback enabled\n")
		fmt.Fprintf(os.Stderr, "  --no-color           suppress ANSI color escape codes (also auto-enabled on non-TTY)\n")
		fmt.Fprintf(os.Stderr, "  --lines N, -n N      backfill last N events before following in pure-tail mode (default 10)\n")
		fmt.Fprintf(os.Stderr, "  --history N          pre-fill TUI with last N events (default 200; 0 = fill visible pane)\n")
		fmt.Fprintf(os.Stderr, "  --exclude TAGS, -x   comma-separated tags to exclude (e.g. EXEC,TOOL)\n")
		fmt.Fprintf(os.Stderr, "  --session PREFIX, -s filter to sessions whose ID starts with PREFIX\n")
	}

	flag.Parse()

	if *flagTail && *flagDashboard {
		slog.Error("--tail and --dashboard are mutually exclusive")
		os.Exit(1)
	}

	if *basedir != "" {
		os.Setenv("TEAMSTER_BASEDIR", *basedir)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "component", "feed", "error", err)
		os.Exit(1)
	}

	logging.Init("feed")

	logPath := cfg.EventLogPath()
	if flag.NArg() > 0 {
		logPath = flag.Arg(0)
	}

	excludeTags := map[string]bool{}
	if *excludeStr != "" {
		for _, t := range strings.Split(*excludeStr, ",") {
			t = strings.TrimSpace(strings.ToUpper(t))
			if t != "" {
				excludeTags[t] = true
			}
		}
	}

	isTTY := term.IsTerminal(os.Stdout.Fd())

	// Mode resolution
	pureTail := false
	dashboardMode := false
	if *flagTail {
		pureTail = true
	} else if *flagDashboard {
		dashboardMode = true
	} else if !isTTY {
		pureTail = true
	}
	// Default (isTTY, no flags): tail-style TUI (header pinned, newest at bottom)

	// Color resolution
	colorize := isTTY && !*flagNoColor

	if pureTail {
		if err := runPureTail(logPath, excludeTags, *sessionFilter, colorize, *flagLines); err != nil {
			slog.Error("run failed", "error", err)
			os.Exit(1)
		}
		return
	}

	history := *historyN
	if history == 0 {
		history = 200
	}

	m, err := newModel(logPath, excludeTags, *sessionFilter, history)
	if err != nil {
		slog.Error("model init failed", "error", err)
		os.Exit(1)
	}
	m.dashboardMode = dashboardMode
	m.colorize = colorize

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		slog.Error("program run failed", "error", err)
		os.Exit(1)
	}
}
