package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/termenv"
)

func main() {
	m := newModel()
	p := tea.NewProgram(m)

	p.EnterAltScreen()
	defer p.ExitAltScreen()

	tea, err := p.Run()
	if err != nil {
		log.Fatalln(err)
	}
	m, ok := tea.(*model)
	if !ok {
		log.Fatalln("expected model type")
	}
	if m.err != nil {
		if m.modules != nil {
			log.Fatalln(m.err)
		}
	}
}

type model struct {
	spinner  spinner.Model
	viewport viewport.Model
	color    termenv.Profile

	builder  strings.Builder
	ready    bool
	modules  []module
	cursor   int
	updating bool

	err error
}

func newModel() *model {
	s := spinner.New()
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	s.Spinner = spinner.Dot

	return &model{
		color:   termenv.ColorProfile(),
		spinner: s,
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		loadCmd(),
	)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		m.err = msg.err
		return m, tea.Quit
	case modulesMsg:
		m.modules = msg.modules
	case updatedMsg:
		m.updating = false
		m.modules = msg.modules
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "enter":
			if !m.updating {
				m.updating = true
				return m, updateCmd(m.modules[m.cursor])
			}
		case "down", "j":
			if !m.updating {
				m.cursor++
				m.fixCursor()
				m.fixViewport(false)
			}
		case "up", "k":
			if !m.updating {
				m.cursor--
				m.fixCursor()
				m.fixViewport(false)
			}
		case "pgup", "u":
			if !m.updating {
				m.viewport.LineUp(1)
				m.fixViewport(true)
			}
		case "pgdown", "d":
			if !m.updating {
				m.viewport.LineDown(1)
				m.fixViewport(true)
			}
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.WindowSizeMsg:
		if !m.ready {
			m.viewport = viewport.Model{
				Width:  msg.Width,
				Height: msg.Height - 2,
			}
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - 2
			m.fixViewport(true)
		}
	}

	return m, nil
}

func (m *model) View() string {
	var header, body, footer string
	if !m.ready || m.modules == nil {
		header = lipgloss.NewStyle().
			Foreground(lipgloss.Color("5")).
			Bold(true).
			Render(m.spinner.View() + " Loading modules...")
	} else if len(m.modules) == 0 {
		header = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2")).
			Bold(true).
			Render("✅ All modules are up-to-date!")
	} else {
		header = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6")).
			Bold(true).
			Render(fmt.Sprintf("Press %s to update [%d/%d]",
				lipgloss.NewStyle().Underline(true).Render("enter"),
				m.cursor+1, len(m.modules)))
		m.viewport.SetContent(m.content())
		body = m.viewport.View()
	}
	footer = lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")).
		Render("(press 'q' to quit)")

	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

func (m *model) content() string {
	defer m.builder.Reset()

	for i, module := range m.modules {
		cursor := " "
		if m.cursor == i {
			cursor = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Render("❯")
			if m.updating {
				cursor = m.spinner.View()
			}
		}

		indirect := ""
		if module.Indirect {
			indirect = lipgloss.NewStyle().
				Foreground(lipgloss.Color("2")).
				Faint(true).
				Render(" // indirect")
		}

		m.builder.WriteString(
			// Truncate long lines if necessary
			truncate.StringWithTail(
				fmt.Sprintf(
					"%s %s [%s -> %s]",
					cursor, module.Path, module.Version, module.Update.Version,
				),

				// We want to always show the indirect portion if it's present,
				// so subtract its width from the window width to get the
				// maximum line width
				uint(m.viewport.Width-ansi.PrintableRuneWidth(indirect)),

				"…",
			) + indirect + "\n",
		)
	}

	return m.builder.String()
}

func (m *model) fixCursor() {
	if m.cursor > len(m.modules)-1 {
		m.cursor = 0
	} else if m.cursor < 0 {
		m.cursor = len(m.modules) - 1
	}
}

func (m *model) fixViewport(moveCursor bool) {
	top := m.viewport.YOffset
	bottom := m.viewport.Height + m.viewport.YOffset - 1

	if moveCursor {
		if m.cursor < top {
			m.cursor = top
		} else if m.cursor > bottom {
			m.cursor = bottom
		}
		return
	}

	if m.cursor < top {
		m.viewport.ScrollUp(top - m.cursor)
	} else if m.cursor > bottom {
		m.viewport.ScrollDown(m.cursor - bottom)
	}
}

type (
	errMsg struct {
		err error
	}
	modulesMsg struct {
		modules []module
	}
	updatedMsg struct {
		modules []module
	}
)

func loadCmd() tea.Cmd {
	return func() tea.Msg {
		modules, err := load()
		if err != nil {
			return errMsg{err}
		}

		return modulesMsg{modules}
	}
}

func updateCmd(m module) tea.Cmd {
	return func() tea.Msg {
		if !m.Indirect {
			cmd := exec.Command("go", "get", "-u", m.Update.Path+"@"+m.Update.Version)
			err := cmd.Run()
			if err != nil {
				return errMsg{err}
			}
		}
		modules, err := load()
		if err != nil {
			return errMsg{err}
		}

		return updatedMsg{modules}
	}
}

type module struct {
	Path     string  `json:"Path"`     // module path
	Version  string  `json:"Version"`  // module version
	Update   *module `json:"Update"`   // available update (with -u)
	Main     bool    `json:"Main"`     // is this the main module?
	Indirect bool    `json:"Indirect"` // module is only indirectly needed by main module
}

func load() ([]module, error) {
	cmd := exec.Command("go", "list", "-m", "-u", "-json", "all")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var (
		modules = make([]module, 0)
		dec     = json.NewDecoder(bytes.NewReader(out))
	)
	for {
		var m module
		err := dec.Decode(&m)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		// not main module, but has an update available
		if !m.Main && m.Update != nil && !m.Indirect {
			modules = append(modules, m)
		}
	}

	sort.Slice(modules, func(i, j int) bool {
		return modules[i].Path < modules[j].Path
	})

	return modules, nil
}
