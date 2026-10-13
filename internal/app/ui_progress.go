package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type uiTickMsg time.Time

func uiTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return uiTickMsg(t) })
}

// Workers never write directly to the TUI terminal. Keep a bounded tail for
// rendering, with complete captured output in the rotating maintenance log.
type uiProgress struct {
	mu   sync.Mutex
	app  *App
	text string
}

func uiPlain(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
func (p *uiProgress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := uiPlain(string(b))
	p.text += s
	if len(p.text) > 32*1024 {
		p.text = p.text[len(p.text)-32*1024:]
		if i := strings.IndexByte(p.text, '\n'); i >= 0 {
			p.text = p.text[i+1:]
		}
	}
	if p.app != nil {
		log := p.app.RuntimeLog("installer")
		_, _ = log.Write([]byte(s))
		_ = log.Close()
	}
	return len(b), nil
}
func (p *uiProgress) reset()           { p.mu.Lock(); defer p.mu.Unlock(); p.text = "" }
func (p *uiProgress) snapshot() string { p.mu.Lock(); defer p.mu.Unlock(); return p.text }
func (m *uiModel) startBusy(stage string) {
	if m.parentCtx != nil {
		m.ctx, m.cancel = context.WithCancel(m.parentCtx)
	}
	m.cancelling = false
	m.busy = true
	m.started = time.Now()
	m.scroll = 0
	if m.progress != nil {
		m.progress.reset()
		_, _ = m.progress.Write([]byte(stage + "\n"))
	}
}

func (m *uiModel) dimensions() (int, int) {
	w, h := m.width, m.height
	if w < 1 {
		w = 100
	}
	if h < 1 {
		h = 30
	}
	return w, h
}
func (m *uiModel) progressView() string {
	w, h := m.dimensions()
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	elapsed := time.Duration(0)
	if !m.started.IsZero() {
		elapsed = time.Since(m.started).Truncate(time.Second)
	}
	head := ansi.Hardwrap(uiTitle.Render(uiPlain(m.title))+"\n\n"+fmt.Sprintf("%s 正在执行 · 已用时 %s", frames[m.frame%len(frames)], elapsed), max(1, w-2), true)
	tail := "正在准备操作…"
	if m.progress != nil {
		tail = m.progress.snapshot()
	}
	lines := strings.Split(ansi.Hardwrap(strings.TrimSpace(tail), max(1, w-2), true), "\n")
	footer := "Ctrl+C 请求取消并等待清理 · 输出记录于 installer-data/logs/installer.log"
	if m.cancelling {
		footer = "正在取消，等待子进程退出和事务恢复…"
	}
	footer = ansi.Hardwrap(footer, max(1, w-2), true)
	available := max(1, h-strings.Count(head, "\n")-strings.Count(footer, "\n")-6)
	if len(lines) > available {
		lines = lines[len(lines)-available:]
	}
	return head + "\n\n" + strings.Join(lines, "\n") + "\n\n" + footer
}

func (m *uiModel) frameView(text, footer string, focus int) string {
	w, h := m.dimensions()
	lines := strings.Split(ansi.Hardwrap(text, max(1, w-2), true), "\n")
	foot := ansi.Hardwrap(footer, max(1, w-2), true)
	limit := max(1, h-strings.Count(foot, "\n")-3)
	if len(lines) > limit {
		foot = ansi.Hardwrap(footer+" · PgUp/PgDn 翻页", max(1, w-2), true)
		limit = max(1, h-strings.Count(foot, "\n")-3)
	}
	if focus >= 0 && !m.manualScroll {
		if focus < m.scroll {
			m.scroll = focus
		}
		if focus >= m.scroll+limit {
			m.scroll = focus - limit + 1
		}
	}
	m.scroll = max(0, min(m.scroll, max(0, len(lines)-limit)))
	end := min(len(lines), m.scroll+limit)
	return strings.Join(lines[m.scroll:end], "\n") + "\n\n" + foot
}
