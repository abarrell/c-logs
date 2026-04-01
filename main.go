package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

const maxLines = 5000

// ANSI helpers.
const (
	reset      = "\033[0m"
	bold       = "\033[1m"
	dim        = "\033[2m"
	italic     = "\033[3m"
	boldGreen  = "\033[1;32m"
	boldYellow = "\033[1;33m"
	boldRed    = "\033[1;31m"
	boldBlue   = "\033[1;34m"
	gray       = "\033[90m"
	inverted   = "\033[7m"
	headerBg   = "\033[48;5;235m" // dark gray background
)

var svcColors = [...]string{
	"\033[36m", "\033[32m", "\033[33m", "\033[35m", "\033[34m",
	"\033[91m", "\033[92m", "\033[93m", "\033[94m", "\033[95m", "\033[96m",
}

// --- Types ---

type logEntry struct {
	service string
	text    string
	ts      time.Time
}

type svcInfo struct {
	name    string
	running bool
}

type model struct {
	composeDir string
	tail       int
	services   []svcInfo
	active     map[string]bool
	cancels    map[string]context.CancelFunc
	lines      []logEntry
	logCh      chan logEntry
	width      int
	height     int
	ready      bool
	quitting   bool

	// Scroll: 0 = pinned to bottom (auto-scroll). >0 = scrolled up by N lines.
	scrollOffset int
	prettyJSON   bool
}

// --- Messages ---

type logMsg logEntry
type servicesMsg []svcInfo

// --- Main ---

func main() {
	tail := 50
	var initial []string

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-h", "--help":
			printUsage()
			os.Exit(0)
		case "-n", "--tail":
			if i+1 < len(os.Args) {
				i++
				if n, err := strconv.Atoi(os.Args[i]); err == nil {
					tail = n
				}
			}
		default:
			if !strings.HasPrefix(os.Args[i], "-") {
				initial = append(initial, os.Args[i])
			}
		}
	}

	composeDir, err := findComposeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	active := make(map[string]bool)
	for _, s := range initial {
		active[s] = true
	}

	m := model{
		composeDir: composeDir,
		tail:       tail,
		active:     active,
		cancels:    make(map[string]context.CancelFunc),
		logCh:      make(chan logEntry, 256),
		prettyJSON: true,
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

const appName = "compose-logs"

func printUsage() {
	fmt.Printf(`%s — interactive Docker Compose log viewer

Usage:
  %s                    start with all running services active
  %s web api            start with specific services active
  %s -n 200             show last 200 lines of history per service

Controls (while running):
  1-9, 0        toggle service by number
  a             activate all services
  n             deactivate all services
  r             activate only running services
  p             toggle JSON pretty-print
  c             clear log output
  up/k          scroll up
  down/j        scroll down
  pgup/pgdn     scroll by page
  G/end         jump to bottom (resume auto-scroll)
  q             quit

Flags:
  -n, --tail NUM    historical lines per service (default: 50)
  -h, --help        show this help
`, appName, appName, appName, appName)
}

// --- Bubbletea lifecycle ---

func (m model) Init() tea.Cmd {
	return tea.Batch(
		loadServices(m.composeDir),
		listenForLog(m.logCh),
	)
}

func loadServices(composeDir string) tea.Cmd {
	return func() tea.Msg {
		all, _ := getAllServices(composeDir)
		running, _ := getRunningServices(composeDir)
		runSet := make(map[string]bool, len(running))
		for _, s := range running {
			runSet[s] = true
		}
		var svcs []svcInfo
		for _, name := range all {
			svcs = append(svcs, svcInfo{name: name, running: runSet[name]})
		}
		return servicesMsg(svcs)
	}
}

func listenForLog(ch <-chan logEntry) tea.Cmd {
	return func() tea.Msg {
		return logMsg(<-ch)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case servicesMsg:
		m.services = []svcInfo(msg)
		m.ready = true
		if len(m.active) == 0 {
			for _, s := range m.services {
				if s.running {
					m.active[s.name] = true
				}
			}
		}
		for name := range m.active {
			startStreaming(m.cancels, m.composeDir, name, m.tail, m.logCh)
		}
		return m, nil

	case logMsg:
		m.lines = append(m.lines, logEntry(msg))
		if len(m.lines) > maxLines {
			m.lines = m.lines[maxLines/5:]
		}
		return m, listenForLog(m.logCh)

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollOffset += 3
			return m, nil
		case tea.MouseButtonWheelDown:
			m.scrollOffset -= 3
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
			return m, nil
		}

	case tea.KeyMsg:
		if !m.ready {
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			stopAllStreaming(m.cancels)
			return m, tea.Quit
		case "up", "k":
			m.scrollOffset++
			return m, nil
		case "down", "j":
			m.scrollOffset--
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
			return m, nil
		case "pgup":
			m.scrollOffset += m.logAreaHeight() / 2
			return m, nil
		case "pgdown":
			m.scrollOffset -= m.logAreaHeight() / 2
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
			return m, nil
		case "G", "end":
			m.scrollOffset = 0
			return m, nil
		case "a":
			for _, s := range m.services {
				m.active[s.name] = true
				startStreaming(m.cancels, m.composeDir, s.name, m.tail, m.logCh)
			}
			return m, nil
		case "n":
			stopAllStreaming(m.cancels)
			m.active = make(map[string]bool)
			return m, nil
		case "r":
			stopAllStreaming(m.cancels)
			m.active = make(map[string]bool)
			for _, s := range m.services {
				if s.running {
					m.active[s.name] = true
					startStreaming(m.cancels, m.composeDir, s.name, m.tail, m.logCh)
				}
			}
			return m, nil
		case "p":
			m.prettyJSON = !m.prettyJSON
			return m, nil
		case "c":
			m.lines = nil
			m.scrollOffset = 0
			return m, nil
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "0":
			idx := int(msg.String()[0] - '1')
			if msg.String() == "0" {
				idx = 9
			}
			if idx >= 0 && idx < len(m.services) {
				name := m.services[idx].name
				if m.active[name] {
					m.active[name] = false
					stopStreaming(m.cancels, name)
				} else {
					m.active[name] = true
					startStreaming(m.cancels, m.composeDir, name, m.tail, m.logCh)
				}
			}
			return m, nil
		}
	}

	return m, nil
}

// --- View ---

func (m model) View() string {
	if m.quitting {
		return ""
	}
	if m.height == 0 || !m.ready {
		return "\n  Loading services...\n"
	}

	// Pre-allocate exactly m.height rows. Fill top-down. Every row is set exactly once.
	output := make([]string, m.height)

	// 1. Header (pinned to top).
	header := m.headerLines()
	copy(output, header)
	row := len(header) // next row to write

	// 2. Log area fills the rest.
	logH := m.height - row
	if logH < 1 {
		logH = 1
	}

	// 3. Build all visual rows from visible log entries.
	//    Each log entry may wrap to multiple visual rows. Scrolling operates
	//    on visual rows so the output always fits exactly m.height terminal lines.
	allVisible := m.getSortedVisible()
	var allRows []string

	if len(allVisible) == 0 {
		allRows = append(allRows,
			fmt.Sprintf("  %s%sSelect services to view their logs.%s", dim, italic, reset),
			fmt.Sprintf("  %s%sPress a number to toggle, or [r] for all running services.%s", dim, italic, reset),
		)
	} else {
		maxName := 0
		for _, s := range m.services {
			if m.active[s.name] && len(s.name) > maxName {
				maxName = len(s.name)
			}
		}
		colorIdx := make(map[string]int, len(m.services))
		for i, s := range m.services {
			colorIdx[s.name] = i
		}
		prefixVisWidth := maxName + 5 // "  " + name(maxName) + " │ "
		indent := "  " + strings.Repeat(" ", maxName) + " " + gray + "│" + reset + " "
		for _, entry := range allVisible {
			ci := colorIdx[entry.service] % len(svcColors)
			var textLines []string
			if m.prettyJSON {
				textLines = formatLogTextPretty(entry.text)
			} else {
				textLines = formatLogTextCompact(entry.text)
			}
			prefix := fmt.Sprintf("  %s%-*s%s %s│%s ",
				svcColors[ci], maxName, entry.service, reset, gray, reset)

			if m.width > 0 && prefixVisWidth < m.width-10 {
				textWidth := m.width - prefixVisWidth
				first := true
				for _, tl := range textLines {
					// Split embedded newlines (from pretty-printed JSON) into separate rows.
					for _, sub := range strings.Split(tl, "\n") {
						wrapped := ansiWrap(sub, textWidth)
						for _, wr := range wrapped {
							if first {
								allRows = append(allRows, prefix+wr)
								first = false
							} else {
								allRows = append(allRows, indent+wr)
							}
						}
					}
				}
			} else {
				allRows = append(allRows, prefix+strings.Join(textLines, " "))
			}
		}
	}

	// 4. Apply scroll window to visual rows.
	total := len(allRows)

	maxOffset := total - logH
	if maxOffset < 0 {
		maxOffset = 0
	}
	offset := m.scrollOffset
	if offset > maxOffset {
		offset = maxOffset
	}

	// Reserve one row for the scroll indicator when scrolled.
	displayH := logH
	if offset > 0 {
		displayH--
	}

	end := total - offset
	start := end - displayH
	if start < 0 {
		start = 0
	}
	window := allRows[start:end]

	// Count total content rows (visible rows + indicator) to compute top padding.
	contentRows := len(window)
	if offset > 0 {
		contentRows++
	}
	padRows := logH - contentRows
	if padRows < 0 {
		padRows = 0
	}

	// Fill output: pad (empty rows), then log rows, then indicator — anchored to bottom.
	row += padRows
	for _, line := range window {
		if row < m.height {
			output[row] = line
			row++
		}
	}
	if offset > 0 && row < m.height {
		output[row] = fmt.Sprintf("  %s%s ↑ %d more lines — press G to jump to latest %s", inverted, gray, offset, reset)
	}

	// Safety: truncate every line to terminal width as a final guarantee against wrapping.
	if m.width > 0 {
		for i, line := range output {
			output[i] = ansiTruncate(line, m.width)
		}
	}

	return strings.Join(output, "\n")
}

// headerLines returns the styled header rows.
func (m model) headerLines() []string {
	w := m.width
	if w < 40 {
		w = 80
	}

	var lines []string

	// Title row.
	lines = append(lines, m.bgPad(fmt.Sprintf("  %s%s%s", boldBlue, appName, reset), w))

	// Service toggles.
	maxName := 0
	for _, s := range m.services {
		if len(s.name) > maxName {
			maxName = len(s.name)
		}
	}
	colWidth := maxName + 10
	cols := w / colWidth
	if cols < 1 {
		cols = 1
	}

	var row strings.Builder
	for i, s := range m.services {
		key := " "
		if i < 9 {
			key = strconv.Itoa(i + 1)
		} else if i == 9 {
			key = "0"
		}
		var bullet, color string
		if m.active[s.name] {
			bullet = "●"
			color = boldGreen
		} else {
			bullet = "○"
			color = gray
		}
		row.WriteString(fmt.Sprintf("  [%s] %s%s %-*s%s", key, color, bullet, maxName, s.name, reset))

		if (i+1)%cols == 0 || i == len(m.services)-1 {
			lines = append(lines, m.bgPad(row.String(), w))
			row.Reset()
		}
	}

	// Controls row.
	maxKey := len(m.services)
	if maxKey > 10 {
		maxKey = 10
	}
	keyRange := "1"
	if maxKey > 1 {
		keyRange = fmt.Sprintf("1-%d", maxKey%10)
		if maxKey == 10 {
			keyRange = "1-0"
		}
	}
	prettyLabel := "json:off"
	if m.prettyJSON {
		prettyLabel = "json:on"
	}
	controls := fmt.Sprintf("  %s%s%s toggle %sa%sll %sn%sone %sr%sunning %sc%slear %sp%s %s %s↑↓%s scroll %sG%s latest %sq%suit",
		bold, keyRange, reset,
		bold, reset, bold, reset, bold, reset, bold, reset,
		bold, reset, prettyLabel,
		bold, reset, bold, reset, bold, reset)
	lines = append(lines, m.bgPad(controls, w))

	// Bottom border (use exact terminal width, not the w variable which may be wider).
	bw := m.width
	if bw < 1 {
		bw = 80
	}
	lines = append(lines, fmt.Sprintf("%s%s%s", gray, strings.Repeat("─", bw), reset))

	return lines
}

// bgPad renders text on the header background, truncated and padded to exactly the terminal width.
func (m model) bgPad(content string, width int) string {
	content = ansiTruncate(content, width)
	vLen := ansiVisualLen(content)
	pad := width - vLen
	if pad < 0 {
		pad = 0
	}
	return headerBg + content + strings.Repeat(" ", pad) + reset
}

// headerRowCount returns the number of rows the header occupies, computed structurally.
func (m model) headerRowCount() int {
	if len(m.services) == 0 {
		return 4 // title + controls + border + blank
	}
	w := m.width
	if w < 40 {
		w = 80
	}
	maxName := 0
	for _, s := range m.services {
		if len(s.name) > maxName {
			maxName = len(s.name)
		}
	}
	colWidth := maxName + 10
	cols := w / colWidth
	if cols < 1 {
		cols = 1
	}
	svcRows := (len(m.services) + cols - 1) / cols
	return 1 + svcRows + 1 + 1 // title + service rows + controls + border
}

// logAreaHeight returns how many terminal rows are available for log output.
func (m model) logAreaHeight() int {
	h := m.height - m.headerRowCount()
	if h < 1 {
		return 1
	}
	return h
}

// sanitizeText replaces tabs and strips control characters that cause incorrect width measurement.
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

// parseSlogJSON tries to parse text as a Go slog JSON log entry.
// Returns the parsed fields, level, and msg, or ok=false if not a slog entry.
func parseSlogJSON(text string) (fields map[string]any, level, msg string, ok bool) {
	text = strings.TrimSpace(text)
	if len(text) == 0 || text[0] != '{' {
		return nil, "", "", false
	}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return nil, "", "", false
	}
	level, _ = fields["level"].(string)
	msg, _ = fields["msg"].(string)
	if msg == "" && level == "" {
		return nil, "", "", false
	}
	return fields, level, msg, true
}

// levelBadge returns a colored 3-char level indicator.
func levelBadge(level string) string {
	switch level {
	case "INFO":
		return boldGreen + "INF" + reset + " "
	case "WARN", "WARNING":
		return boldYellow + "WRN" + reset + " "
	case "ERROR":
		return boldRed + "ERR" + reset + " "
	case "DEBUG":
		return gray + "DBG" + reset + " "
	default:
		if level != "" {
			return bold + level + reset + " "
		}
		return ""
	}
}

// sortedExtraKeys returns field keys excluding time/level/msg, sorted.
func sortedExtraKeys(fields map[string]any) []string {
	skip := map[string]bool{"time": true, "level": true, "msg": true}
	var keys []string
	for k := range fields {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// fieldValueString returns a string representation of a field value.
func fieldValueString(v any) string {
	switch vt := v.(type) {
	case string:
		return vt
	default:
		jb, _ := json.Marshal(v)
		return string(jb)
	}
}

// formatLogTextCompact formats a slog JSON entry as a single line:
// INF msg  key=value  key=value
func formatLogTextCompact(text string) []string {
	fields, level, msg, ok := parseSlogJSON(text)
	if !ok {
		return []string{text}
	}

	var b strings.Builder
	b.WriteString(levelBadge(level))
	b.WriteString(msg)

	for _, k := range sortedExtraKeys(fields) {
		b.WriteString("  " + dim + k + "=" + reset + fieldValueString(fields[k]))
	}

	return []string{b.String()}
}

// formatLogTextPretty formats a slog JSON entry as multiple lines with
// indented, pretty-printed JSON values.
func formatLogTextPretty(text string) []string {
	fields, level, msg, ok := parseSlogJSON(text)
	if !ok {
		return []string{text}
	}

	lines := []string{levelBadge(level) + msg}

	for _, k := range sortedExtraKeys(fields) {
		v := fields[k]
		var vs string
		switch vt := v.(type) {
		case string:
			// Try to pretty-print if the string value is itself JSON.
			if len(vt) > 0 && (vt[0] == '{' || vt[0] == '[') {
				var inner any
				if err := json.Unmarshal([]byte(vt), &inner); err == nil {
					pretty, err := json.MarshalIndent(inner, "    ", "  ")
					if err == nil {
						vs = "\n    " + string(pretty)
					}
				}
			}
			if vs == "" {
				vs = vt
			}
		default:
			pretty, err := json.MarshalIndent(v, "    ", "  ")
			if err == nil {
				vs = "\n    " + string(pretty)
			} else {
				vs = fieldValueString(v)
			}
		}
		lines = append(lines, "  "+dim+k+"="+reset+vs)
	}

	return lines
}

// ANSI escape parser states.
const (
	escNone = iota // normal text
	escSaw         // saw \033, waiting for next char
	escCSI         // inside CSI: \033[ ... <letter>
	escOSC         // inside OSC: \033] ... (ST or BEL)
)

// isAnsiEscape reports whether r is part of an escape sequence given the
// current parser state, and returns the next state.
func nextEscState(state int, r rune) (next int, isEsc bool) {
	switch state {
	case escNone:
		if r == '\033' {
			return escSaw, true
		}
		return escNone, false
	case escSaw:
		if r == '[' {
			return escCSI, true
		}
		if r == ']' {
			return escOSC, true
		}
		// Single-char escape (e.g. \033(B) — consume this char and done.
		return escNone, true
	case escCSI:
		// CSI params: digits, semicolons, intermediates (0x20-0x2F), terminated by 0x40-0x7E.
		if r >= 0x40 && r <= 0x7E {
			return escNone, true
		}
		return escCSI, true
	case escOSC:
		// OSC terminated by BEL (\007) or ST (\033\\, handled as new escSaw).
		if r == '\007' {
			return escNone, true
		}
		if r == '\033' {
			return escSaw, true
		}
		return escOSC, true
	}
	return escNone, false
}

// ansiVisualLen returns the terminal cell width of a string, skipping ANSI escape sequences.
func ansiVisualLen(s string) int {
	n := 0
	state := escNone
	for _, r := range s {
		var esc bool
		state, esc = nextEscState(state, r)
		if esc {
			continue
		}
		n += runewidth.RuneWidth(r)
	}
	return n
}

// ansiWrap splits s into lines of at most maxWidth terminal cells.
// ANSI state is reset at each line break. If s fits in one line, it is returned as-is.
func ansiWrap(s string, maxWidth int) []string {
	if maxWidth <= 0 || ansiVisualLen(s) <= maxWidth {
		return []string{s}
	}

	var rows []string
	var cur strings.Builder
	vis := 0
	state := escNone

	for _, r := range s {
		var esc bool
		state, esc = nextEscState(state, r)
		if esc {
			cur.WriteRune(r)
			continue
		}

		w := runewidth.RuneWidth(r)
		if vis+w > maxWidth {
			cur.WriteString(reset)
			rows = append(rows, cur.String())
			cur.Reset()
			vis = 0
		}
		cur.WriteRune(r)
		vis += w
	}
	if cur.Len() > 0 {
		rows = append(rows, cur.String())
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	return rows
}

// ansiTruncate truncates s to at most maxWidth terminal cells, preserving ANSI codes.
func ansiTruncate(s string, maxWidth int) string {
	var out strings.Builder
	vis := 0
	state := escNone
	for _, r := range s {
		var esc bool
		state, esc = nextEscState(state, r)
		if esc {
			out.WriteRune(r)
			continue
		}
		w := runewidth.RuneWidth(r)
		if vis+w > maxWidth {
			break
		}
		out.WriteRune(r)
		vis += w
	}
	return out.String()
}

// getSortedVisible returns all log lines from active services, sorted by timestamp.
func (m model) getSortedVisible() []logEntry {
	var all []logEntry
	for _, entry := range m.lines {
		if m.active[entry.service] {
			all = append(all, entry)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].ts.Before(all[j].ts)
	})
	return all
}

// --- Streaming ---

func startStreaming(cancels map[string]context.CancelFunc, composeDir, name string, tail int, ch chan<- logEntry) {
	if _, exists := cancels[name]; exists {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancels[name] = cancel
	go streamServiceLogs(ctx, composeDir, name, tail, ch)
}

func stopStreaming(cancels map[string]context.CancelFunc, name string) {
	if cancel, ok := cancels[name]; ok {
		cancel()
		delete(cancels, name)
	}
}

func stopAllStreaming(cancels map[string]context.CancelFunc) {
	for name, cancel := range cancels {
		cancel()
		delete(cancels, name)
	}
}

func streamServiceLogs(ctx context.Context, composeDir, svc string, tail int, ch chan<- logEntry) {
	args := []string{"compose", "logs", "-f", "--timestamps", "--tail", strconv.Itoa(tail), "--no-log-prefix", svc}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = composeDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return
	}
	defer cmd.Wait() //nolint:errcheck

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	for scanner.Scan() {
		ts, text := parseTimestamp(scanner.Text())
		select {
		case ch <- logEntry{service: svc, text: text, ts: ts}:
		case <-ctx.Done():
			return
		}
	}
}

func parseTimestamp(line string) (time.Time, string) {
	if idx := strings.IndexByte(line, ' '); idx > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, line[:idx]); err == nil {
			return ts, sanitizeText(line[idx+1:])
		}
	}
	return time.Now(), sanitizeText(line)
}

// --- Docker Compose ---

func getAllServices(composeDir string) ([]string, error) {
	cmd := exec.Command("docker", "compose", "config", "--services")
	cmd.Dir = composeDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			services = append(services, line)
		}
	}
	sort.Strings(services)
	return services, nil
}

func getRunningServices(composeDir string) ([]string, error) {
	cmd := exec.Command("docker", "compose", "ps", "--format", "{{.Service}}")
	cmd.Dir = composeDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var services []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			services = append(services, line)
		}
	}
	sort.Strings(services)
	return services, nil
}

func findComposeDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	d := cwd
	for {
		for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				return d, nil
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", fmt.Errorf("no compose file found in any parent directory")
}
