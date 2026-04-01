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
	service    string
	text       string
	ts         time.Time
	fmtCompact []string // cached formatLogTextCompact result
	fmtPretty  []string // cached formatLogTextPretty result
}

type svcInfo struct {
	name    string
	running bool
}

// backend abstracts service discovery and log streaming for different Docker modes.
type backend interface {
	// AllServices returns all known service/container names.
	AllServices() ([]string, error)
	// RunningServices returns only running service/container names.
	RunningServices() ([]string, error)
	// StreamLogs streams logs for the named service into ch. Blocks until ctx is cancelled.
	StreamLogs(ctx context.Context, name string, tail int, ch chan<- logEntry)
}

type model struct {
	backend backend
	tail    int

	services     []svcInfo
	active       map[string]bool
	cancels      map[string]context.CancelFunc
	lines        []logEntry
	logCh        chan logEntry
	width        int
	height       int
	ready        bool
	quitting     bool
	scrollOffset int
	prettyJSON   bool

	// Service picker dropdown.
	pickerOpen   bool
	pickerCursor int
	pickerScroll int // first visible index in the picker list

	// Tracks services that have been streamed at least once, to avoid
	// re-fetching tail history and duplicating lines on re-activation.
	streamed map[string]bool

	// Render cache – rebuilt only when visibleDirty is true.
	visibleCache []logEntry
	visibleDirty bool
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

	var be backend
	if dir, err := findComposeDir(); err == nil {
		be = &composeBackend{dir: dir}
	} else {
		be = &dockerBackend{}
	}

	active := make(map[string]bool)
	for _, s := range initial {
		active[s] = true
	}

	m := model{
		backend:    be,
		tail:       tail,
		active:     active,
		cancels:    make(map[string]context.CancelFunc),
		logCh:      make(chan logEntry, 256),
		prettyJSON: true,
		streamed:   make(map[string]bool),
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

const appName = "compose-logs"

func printUsage() {
	fmt.Printf(`%s — interactive Docker log viewer

Usage:
  %s                    start with all running services active
  %s web api            start with specific services active
  %s -n 200             show last 200 lines of history per service

Auto-detects Docker Compose (if a compose file exists) or falls back to plain Docker containers.

Controls (while running):
  1-9, 0        toggle service by number
  tab           open service picker (↑↓ navigate, space/enter toggle, esc close)
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
		loadServices(m.backend),
		listenForLog(m.logCh),
	)
}

func loadServices(be backend) tea.Cmd {
	return func() tea.Msg {
		all, _ := be.AllServices()
		running, _ := be.RunningServices()
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
	newM, cmd := m.update(msg)
	mm := newM.(model)
	if mm.visibleDirty {
		mm.rebuildVisibleCache()
	}
	return mm, cmd
}

func (m *model) rebuildVisibleCache() {
	n := 0
	for _, entry := range m.lines {
		if m.active[entry.service] {
			n++
		}
	}
	all := make([]logEntry, 0, n)
	for _, entry := range m.lines {
		if m.active[entry.service] {
			all = append(all, entry)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].ts.Before(all[j].ts)
	})
	m.visibleCache = all
	m.visibleDirty = false
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
			startStreaming(m.cancels, m.streamed, m.backend, name, m.tail, m.logCh)
		}
		m.visibleDirty = true
		return m, nil

	case logMsg:
		m.lines = append(m.lines, logEntry(msg))
		if len(m.lines) > maxLines {
			m.lines = m.lines[maxLines/5:]
		}
		m.visibleDirty = true
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

		// When picker is open, capture keys for it.
		if m.pickerOpen {
			switch msg.String() {
			case "shift+tab", "tab", "esc":
				m.pickerOpen = false
				return m, nil
			case "q", "ctrl+c":
				m.pickerOpen = false
				m.quitting = true
				stopAllStreaming(m.cancels)
				return m, tea.Quit
			case "up", "k":
				if m.pickerCursor > 0 {
					m.pickerCursor--
					if m.pickerCursor < m.pickerScroll {
						m.pickerScroll = m.pickerCursor
					}
				}
				return m, nil
			case "down", "j":
				if m.pickerCursor < len(m.services)-1 {
					m.pickerCursor++
					maxVis := m.pickerMaxVisible()
					if m.pickerCursor >= m.pickerScroll+maxVis {
						m.pickerScroll = m.pickerCursor - maxVis + 1
					}
				}
				return m, nil
			case " ", "enter":
				if m.pickerCursor >= 0 && m.pickerCursor < len(m.services) {
					name := m.services[m.pickerCursor].name
					if m.active[name] {
						m.active[name] = false
						stopStreaming(m.cancels, name)
					} else {
						m.active[name] = true
						startStreaming(m.cancels, m.streamed, m.backend, name, m.tail, m.logCh)
					}
					m.visibleDirty = true
				}
				return m, nil
			case "a":
				for _, s := range m.services {
					m.active[s.name] = true
					startStreaming(m.cancels, m.streamed, m.backend, s.name, m.tail, m.logCh)
				}
				m.visibleDirty = true
				return m, nil
			case "n":
				stopAllStreaming(m.cancels)
				m.active = make(map[string]bool)
				m.visibleDirty = true
				return m, nil
			}
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
				startStreaming(m.cancels, m.streamed, m.backend, s.name, m.tail, m.logCh)
			}
			m.visibleDirty = true
			return m, nil
		case "n":
			stopAllStreaming(m.cancels)
			m.active = make(map[string]bool)
			m.visibleDirty = true
			return m, nil
		case "r":
			stopAllStreaming(m.cancels)
			m.active = make(map[string]bool)
			for _, s := range m.services {
				if s.running {
					m.active[s.name] = true
					startStreaming(m.cancels, m.streamed, m.backend, s.name, m.tail, m.logCh)
				}
			}
			m.visibleDirty = true
			return m, nil
		case "p":
			m.prettyJSON = !m.prettyJSON
			return m, nil
		case "c":
			m.lines = nil
			m.scrollOffset = 0
			m.visibleDirty = true
			return m, nil
		case "shift+tab", "tab":
			m.pickerOpen = true
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
					startStreaming(m.cancels, m.streamed, m.backend, name, m.tail, m.logCh)
				}
				m.visibleDirty = true
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

	// 3. Build visual rows for the viewport only.
	//    Each log entry may wrap to multiple visual rows. We first compute row
	//    counts per entry to find the viewport window, then only build actual
	//    row strings for entries that are on screen.
	allVisible := m.getSortedVisible()

	if len(allVisible) == 0 {
		emptyRows := []string{
			fmt.Sprintf("  %s%sSelect services to view their logs.%s", dim, italic, reset),
			fmt.Sprintf("  %s%sPress a number to toggle, or [r] for all running services.%s", dim, italic, reset),
		}
		padRows := logH - len(emptyRows)
		if padRows < 0 {
			padRows = 0
		}
		row += padRows
		for _, line := range emptyRows {
			if row < m.height {
				output[row] = line
				row++
			}
		}
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
		canWrap := m.width > 0 && prefixVisWidth < m.width-10
		textWidth := 0
		if canWrap {
			textWidth = m.width - prefixVisWidth
		}

		// Phase 1: compute visual row count per entry (cheap — no string building).
		rowCounts := make([]int, len(allVisible))
		total := 0
		for i, entry := range allVisible {
			var textLines []string
			if m.prettyJSON {
				textLines = entry.fmtPretty
			} else {
				textLines = entry.fmtCompact
			}
			if canWrap {
				rowCounts[i] = entryVisualRows(textLines, textWidth)
			} else {
				rowCounts[i] = 1
			}
			total += rowCounts[i]
		}

		// Phase 2: compute scroll window in visual-row space.
		maxOffset := total - logH
		if maxOffset < 0 {
			maxOffset = 0
		}
		offset := m.scrollOffset
		if offset > maxOffset {
			offset = maxOffset
		}
		displayH := logH
		if offset > 0 {
			displayH--
		}
		endRow := total - offset
		startRow := endRow - displayH
		if startRow < 0 {
			startRow = 0
		}

		// Phase 3: find which entries overlap the viewport [startRow, endRow).
		cumRow := 0
		firstEntry := -1
		skipRowsInFirst := 0
		lastEntry := -1
		for i, rc := range rowCounts {
			nextCum := cumRow + rc
			if firstEntry == -1 && nextCum > startRow {
				firstEntry = i
				skipRowsInFirst = startRow - cumRow
			}
			if nextCum >= endRow {
				lastEntry = i
				break
			}
			cumRow = nextCum
		}
		if firstEntry == -1 {
			firstEntry = 0
		}
		if lastEntry == -1 {
			lastEntry = len(allVisible) - 1
		}

		// Phase 4: build row strings only for viewport entries.
		indent := "  " + strings.Repeat(" ", maxName) + " " + gray + "│" + reset + " "
		var window []string
		for ei := firstEntry; ei <= lastEntry; ei++ {
			entry := allVisible[ei]
			ci := colorIdx[entry.service] % len(svcColors)
			var textLines []string
			if m.prettyJSON {
				textLines = entry.fmtPretty
			} else {
				textLines = entry.fmtCompact
			}
			prefix := fmt.Sprintf("  %s%-*s%s %s│%s ",
				svcColors[ci], maxName, entry.service, reset, gray, reset)

			var entryRows []string
			if canWrap {
				first := true
				for _, tl := range textLines {
					for _, sub := range strings.Split(tl, "\n") {
						wrapped := ansiWrap(sub, textWidth)
						for _, wr := range wrapped {
							if first {
								entryRows = append(entryRows, prefix+wr)
								first = false
							} else {
								entryRows = append(entryRows, indent+wr)
							}
						}
					}
				}
			} else {
				entryRows = append(entryRows, prefix+strings.Join(textLines, " "))
			}

			// Trim rows outside the viewport for first/last entry.
			trimStart := 0
			if ei == firstEntry {
				trimStart = skipRowsInFirst
			}
			trimEnd := len(entryRows)
			remaining := displayH - len(window)
			if trimEnd-trimStart > remaining {
				trimEnd = trimStart + remaining
			}
			window = append(window, entryRows[trimStart:trimEnd]...)
		}

		// Fill output: pad then log rows then indicator — anchored to bottom.
		contentRows := len(window)
		if offset > 0 {
			contentRows++
		}
		padRows := logH - contentRows
		if padRows < 0 {
			padRows = 0
		}
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
	}

	// Overlay the service picker dropdown when open.
	if m.pickerOpen && len(m.services) > 0 {
		maxVis := m.pickerMaxVisible()
		endIdx := m.pickerScroll + maxVis
		if endIdx > len(m.services) {
			endIdx = len(m.services)
		}

		// Find the longest service name for sizing.
		maxName := 0
		for _, s := range m.services {
			if len(s.name) > maxName {
				maxName = len(s.name)
			}
		}

		// Build a sample content row to measure the exact inner visual width.
		// Content between │…│: " ▸ ● <name padded to maxName> "
		sampleInner := " ▸ ● " + strings.Repeat("x", maxName) + " "
		innerW := ansiVisualLen(sampleInner)

		startRow := len(header) // overlay begins right below the header

		// Top border.
		if startRow < m.height {
			line := fmt.Sprintf("  %s%s┌%s┐%s", headerBg, gray, strings.Repeat("─", innerW), reset)
			output[startRow] = line + clearToEnd(line, m.width)
			startRow++
		}

		// Service rows.
		for vi := m.pickerScroll; vi < endIdx && startRow < m.height; vi++ {
			s := m.services[vi]
			cur := " "
			if vi == m.pickerCursor {
				cur = "▸"
			}
			var bullet, color string
			if m.active[s.name] {
				bullet = "●"
				color = boldGreen
			} else {
				bullet = "○"
				color = gray
			}
			namePad := strings.Repeat(" ", maxName-len(s.name))
			line := fmt.Sprintf("  %s%s│%s %s %s%s%s%s %s%s│%s",
				headerBg, gray, reset+headerBg, cur, color, bullet, " "+s.name+namePad, reset+headerBg, gray, reset+headerBg, reset)
			output[startRow] = line + clearToEnd(line, m.width)
			startRow++
		}

		// Bottom border with optional scroll info.
		scrollInfo := ""
		if m.pickerScroll > 0 || endIdx < len(m.services) {
			scrollInfo = fmt.Sprintf(" %d/%d ", endIdx, len(m.services))
		}
		dashW := innerW - len(scrollInfo)
		if dashW < 0 {
			dashW = 0
		}
		if startRow < m.height {
			line := fmt.Sprintf("  %s%s└%s%s┘%s", headerBg, gray, strings.Repeat("─", dashW)+scrollInfo, gray, reset)
			output[startRow] = line + clearToEnd(line, m.width)
		}
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
		// Build entry and pad name manually to avoid %-*s miscounting unicode.
		namePad := strings.Repeat(" ", maxName-len(s.name))
		row.WriteString(fmt.Sprintf("  [%s] %s%s %s%s%s", key, color, bullet, s.name, namePad, reset))

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
	controls := fmt.Sprintf("  %s%s%s toggle %stab%s picker %sa%sll %sn%sone %sr%sunning %sc%slear %sp%s %s %s↑↓%s scroll %sG%s latest %sq%suit",
		bold, keyRange, reset,
		bold, reset,
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

// pickerMaxVisible returns the max number of services visible in the picker dropdown.
func (m model) pickerMaxVisible() int {
	maxH := m.height - m.headerRowCount() - 2 // 2 for picker border top/bottom
	if maxH < 3 {
		maxH = 3
	}
	if maxH > len(m.services) {
		maxH = len(m.services)
	}
	return maxH
}

// clearToEnd pads the overlay line to cover any underlying content up to the terminal width.
func clearToEnd(overlay string, width int) string {
	visLen := ansiVisualLen(overlay)
	pad := width - visLen
	if pad <= 0 {
		return ""
	}
	return strings.Repeat(" ", pad)
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

// entryVisualRows returns the number of visual (terminal) rows that a log entry
// will occupy once formatted and wrapped to the given text width.
func entryVisualRows(textLines []string, textWidth int) int {
	count := 0
	for _, tl := range textLines {
		for _, sub := range strings.Split(tl, "\n") {
			vlen := ansiVisualLen(sub)
			if textWidth > 0 && vlen > textWidth {
				count += (vlen + textWidth - 1) / textWidth
			} else {
				count++
			}
		}
	}
	if count == 0 {
		count = 1
	}
	return count
}

// getSortedVisible returns the cached sorted visible log entries.
// The cache is rebuilt in Update() via rebuildVisibleCache() whenever visibleDirty is set.
func (m model) getSortedVisible() []logEntry {
	return m.visibleCache
}

// --- Streaming ---

func startStreaming(cancels map[string]context.CancelFunc, streamed map[string]bool, be backend, name string, tail int, ch chan<- logEntry) {
	if _, exists := cancels[name]; exists {
		return
	}
	t := tail
	if streamed[name] {
		t = 0 // already have history, only stream new lines
	}
	streamed[name] = true
	ctx, cancel := context.WithCancel(context.Background())
	cancels[name] = cancel
	go be.StreamLogs(ctx, name, t, ch)
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

func parseTimestamp(line string) (time.Time, string) {
	if idx := strings.IndexByte(line, ' '); idx > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, line[:idx]); err == nil {
			return ts, sanitizeText(line[idx+1:])
		}
	}
	return time.Now(), sanitizeText(line)
}

func streamCmd(ctx context.Context, cmd *exec.Cmd, svc string, ch chan<- logEntry) {
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
		entry := logEntry{service: svc, text: text, ts: ts}
		entry.fmtCompact = formatLogTextCompact(text)
		entry.fmtPretty = formatLogTextPretty(text)
		select {
		case ch <- entry:
		case <-ctx.Done():
			return
		}
	}
}

func splitLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// --- Docker Compose backend ---

type composeBackend struct {
	dir string
}

func (b *composeBackend) AllServices() ([]string, error) {
	cmd := exec.Command("docker", "compose", "config", "--services")
	cmd.Dir = b.dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	svcs := splitLines(string(out))
	sort.Strings(svcs)
	return svcs, nil
}

func (b *composeBackend) RunningServices() ([]string, error) {
	cmd := exec.Command("docker", "compose", "ps", "--format", "{{.Service}}")
	cmd.Dir = b.dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var svcs []string
	for _, line := range splitLines(string(out)) {
		if !seen[line] {
			seen[line] = true
			svcs = append(svcs, line)
		}
	}
	sort.Strings(svcs)
	return svcs, nil
}

func (b *composeBackend) StreamLogs(ctx context.Context, name string, tail int, ch chan<- logEntry) {
	args := []string{"compose", "logs", "-f", "--timestamps", "--tail", strconv.Itoa(tail), "--no-log-prefix", name}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = b.dir
	streamCmd(ctx, cmd, name, ch)
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

// --- Plain Docker backend ---

type dockerBackend struct{}

func (b *dockerBackend) AllServices() ([]string, error) {
	// List all containers (running and stopped), using container name as the service identifier.
	cmd := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	svcs := splitLines(string(out))
	sort.Strings(svcs)
	return svcs, nil
}

func (b *dockerBackend) RunningServices() ([]string, error) {
	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	svcs := splitLines(string(out))
	sort.Strings(svcs)
	return svcs, nil
}

func (b *dockerBackend) StreamLogs(ctx context.Context, name string, tail int, ch chan<- logEntry) {
	args := []string{"logs", "-f", "--timestamps", "--tail", strconv.Itoa(tail), name}
	cmd := exec.CommandContext(ctx, "docker", args...)
	streamCmd(ctx, cmd, name, ch)
}
