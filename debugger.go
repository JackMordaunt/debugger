package main

import (
	"fmt"
	"log"
	"os"
	"path"
	"strings"

	"gioui.org/io/system"
	. "github.com/emad-elsaid/types"
	"github.com/go-delve/delve/pkg/gobuild"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/debugger"
)

type DebuggerState uint64

type Debugger struct {
	*debugger.Debugger
	State     DebuggerState
	LastState *api.DebuggerState

	SessionID string

	// Project props
	ProjectPath    string
	Path           string
	BinName        string
	Args           string
	RunImmediately bool
	Test           bool

	// Run state
	StackFrame int
	File       string
	Line       int

	// Cached items
	breakpoints []*api.Breakpoint

	// Watches
	WatchesExpr []string
}

func NewDebugger(p string, bin string, args string, runImmediately bool, test bool) (*Debugger, error) {
	s, err := os.Stat(p)
	if err != nil {
		return nil, err
	}

	if !s.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", p)
	}

	d := Debugger{
		ProjectPath:    ProjectDir(p),
		Path:           p,
		BinName:        bin,
		Args:           args,
		RunImmediately: runImmediately,
		breakpoints:    []*api.Breakpoint{},
		Test:           test,
	}

	if err := d.InitDebugger(); err != nil {
		return nil, err
	}

	if d.RunImmediately {
		d.Continue()
	}

	return &d, nil
}

func (d *Debugger) InitDebugger() error {
	config := d.DebugConfig()
	argsArr := append([]string{path.Join(d.Path, d.BinName)}, strings.Split(d.Args, " ")...)

	if err := d.Compile(); err != nil {
		return err
	}

	deb, err := debugger.New(config, argsArr)
	if err != nil {
		return err
	}

	d.Debugger = deb

	return nil
}

func (d *Debugger) DebugConfig() *debugger.Config {
	executeKind := debugger.ExecutingGeneratedFile

	if d.Test {
		executeKind = debugger.ExecutingGeneratedTest
	}

	return &debugger.Config{
		WorkingDir:  d.Path,
		Backend:     "default",
		Foreground:  true,
		ExecuteKind: executeKind,
		Packages:    []string{d.Path},
	}
}

func (d *Debugger) Stacktrace() ([]proc.Stackframe, error) {
	if d.LastState != nil && d.LastState.Exited {
		return nil, ErrProcessExited
	}

	g, err := d.SelectedGoRoutine()
	if err != nil {
		return nil, err
	}

	if !d.TryLockTarget() {
		return nil, ErrCantInspectTargetProcess
	}
	defer d.UnlockTarget()

	return g.Stacktrace(STACK_LIMIT, proc.StacktraceSimple)
}

func (d *Debugger) CreateBreakpoint(bp *api.Breakpoint) (*api.Breakpoint, error) {
	if d.IsRunning() {
		d.Stop()
		defer d.Continue()
	}

	b, err := d.Debugger.CreateBreakpoint(bp)
	SaveSession(ToSession(d))
	return b, err
}

func (d *Debugger) AmendBreakpoint(bp *api.Breakpoint) error {
	if d.IsRunning() {
		d.Stop()
		defer d.Continue()
	}

	err := d.Debugger.AmendBreakpoint(bp)
	SaveSession(ToSession(d))
	return err
}

func (d *Debugger) ClearBreakpoint(bp *api.Breakpoint) (*api.Breakpoint, error) {
	if d.IsRunning() {
		d.Stop()
		defer d.Continue()
	}

	b, err := d.Debugger.ClearBreakpoint(bp)
	SaveSession(ToSession(d))
	return b, err
}

func (d *Debugger) ClearAllBreakpoints() error {
	if d.IsRunning() {
		d.Stop()
		defer d.Continue()
	}

	for _, bp := range d.Breakpoints() {
		if _, err := d.Debugger.ClearBreakpoint(bp); err != nil {
			return err
		}
	}

	SaveSession(ToSession(d))
	return nil
}

func (d *Debugger) ToggleBreakpoint(bp *api.Breakpoint) error {
	bp.Disabled = !bp.Disabled
	return d.AmendBreakpoint(bp)
}

func (d *Debugger) EnableAllBreakpoints() error {
	if d.IsRunning() {
		d.Stop()
		defer d.Continue()
	}

	for _, bp := range d.Breakpoints() {
		if !bp.Disabled {
			continue
		}
		bp.Disabled = false
		if err := d.Debugger.AmendBreakpoint(bp); err != nil {
			return err
		}
	}

	SaveSession(ToSession(d))
	return nil
}

func (d *Debugger) DisableAllBreakpoints() error {
	if d.IsRunning() {
		d.Stop()
		defer d.Continue()
	}

	for _, bp := range d.Breakpoints() {
		if bp.Disabled {
			continue
		}
		bp.Disabled = true
		if err := d.Debugger.AmendBreakpoint(bp); err != nil {
			return err
		}
	}

	SaveSession(ToSession(d))
	return nil
}

func (d *Debugger) ExecuteExpr(expr string) (*proc.Variable, error) {
	if !d.TryLockTarget() {
		return nil, ErrCantInspectTargetProcess
	}
	defer d.UnlockTarget()

	scope, err := proc.ConvertEvalScope(d.Target(), -1, d.StackFrame, 0)
	if err != nil {
		return nil, ErrCantGetScope
	}

	v, err := scope.EvalExpression(expr, ProcLoadConfig)
	if err != nil {
		return nil, err
	}

	return v, nil
}

func (d *Debugger) SetFileLine(file string, line int) {
	d.File = file
	d.Line = line
	d.InvalidateState()
}

func (d *Debugger) InvalidateState() {
	d.State++
}

func (d *Debugger) Command(cmd *api.DebuggerCommand) {
	state, _ := d.Debugger.Command(cmd, nil)
	if state == nil {
		return
	}

	d.LastState = state
	d.InvalidateState()

	if state.NextInProgress {
		if err := d.CancelNext(); err != nil {
			log.Printf("Error cancelling next: %s", err)
		}
	}

	for _, thread := range state.Threads {
		if thread.Breakpoint != nil {
			file := thread.Breakpoint.File
			line := thread.Breakpoint.Line
			d.SetFileLine(file, line)
			win.Perform(system.ActionRaise)
			break
		}
	}

	if cmd.Name == api.Step ||
		cmd.Name == api.StepOut ||
		cmd.Name == api.Next {
		g, err := d.SelectedGoRoutine()
		if err != nil {
			return
		}

		file := g.CurrentLoc.File
		line := g.CurrentLoc.Line
		d.SetFileLine(file, line)
		win.Perform(system.ActionRaise)
	}

	// If the process exited load it again into the debugger.
	// this allow the user to manipulate the breakpoints after exiting the program
	if state.Exited {
		d.Debugger.Restart(false, "", false, []string{}, [3]string{}, false)
	}
}

func (d *Debugger) SelectedGoRoutine() (*proc.G, error) {
	if !d.TryLockTarget() {
		return nil, ErrCantInspectTargetProcess
	}
	defer d.UnlockTarget()

	t := d.Target()
	g := t.SelectedGoroutine()
	if g == nil {
		routines, _, _ := proc.GoroutinesInfo(t, 0, 0)
		for _, g = range routines {
			break
		}
	}

	if g == nil {
		return nil, ErrNoSelectedGoRoutine
	}

	return g, nil
}

func (d *Debugger) GoRoutines() ([]*proc.G, error) {
	if !d.TryLockTarget() {
		return nil, ErrCantInspectTargetProcess
	}
	defer d.UnlockTarget()

	t := d.Target()
	var routines Slice[*proc.G]
	routines, _, err := proc.GoroutinesInfo(t, 0, 0)
	if err != nil {
		return nil, err
	}

	routines = routines.Select(func(g *proc.G) bool { return !g.System(t) })

	return routines, nil
}

func (d *Debugger) Continue() {
	if d.IsRunning() {
		return
	}

	go d.Command(&api.DebuggerCommand{Name: api.Continue})
}

func (d *Debugger) Stop() {
	d.Command(&api.DebuggerCommand{Name: api.Halt})
}

func (d *Debugger) Restart() {
	d.Stop()

	_, err := d.Debugger.Restart(false, "", false, []string{}, [3]string{}, true)
	if err != nil {
		log.Printf("Error restarting: %s", err)
		return
	}

	go d.Continue()
}

func (d *Debugger) StepOut() {
	if d.IsRunning() {
		return
	}

	go d.Command(&api.DebuggerCommand{Name: api.StepOut})
}

func (d *Debugger) Next() {
	if d.IsRunning() {
		return
	}
	go d.Command(&api.DebuggerCommand{Name: api.Next})
}

func (d *Debugger) Step() {
	if d.IsRunning() {
		return
	}

	go d.Command(&api.DebuggerCommand{Name: api.Step})
}

func (d *Debugger) JumpToFunction(name string) {
	bi := d.Target().BinInfo()
	pc, err := proc.FindFunctionLocation(d.Target().Process, name, 0)
	if err != nil {
		return
	}

	for _, v := range pc {
		file, line, _ := bi.PCToLine(v)
		if file != "" {
			d.SetFileLine(file, line)
			return
		}
	}
}

func (d *Debugger) Compile() error {
	config := d.DebugConfig()
	binPath := path.Join(d.Path, d.BinName)

	if err := os.Chdir(d.Path); err != nil {
		return err
	}

	if _, err := os.Stat(binPath); err == nil {
		gobuild.Remove(binPath)
	}

	var err error
	if d.Test {
		err = gobuild.GoTestBuild(d.BinName, config.Packages, config.BuildFlags)
	} else {
		err = gobuild.GoBuild(d.BinName, config.Packages, config.BuildFlags)
	}

	if err != nil {
		return err
	}

	return nil
}

func (d *Debugger) Breakpoints() []*api.Breakpoint {
	if !d.TryLockTarget() {
		return d.breakpoints
	}
	defer d.UnlockTarget()

	abps := []*api.Breakpoint{}
	for _, lbp := range d.Target().Breakpoints().Logical {
		abp := api.ConvertLogicalBreakpoint(lbp)
		api.ConvertPhysicalBreakpoints(abp, d.findBreakpoint(lbp.LogicalID))
		abps = append(abps, abp)
	}

	d.breakpoints = abps

	return d.breakpoints
}

func (d *Debugger) findBreakpoint(id int) []*proc.Breakpoint {
	var bps []*proc.Breakpoint
	for _, bp := range d.Target().Breakpoints().M {
		if bp.LogicalID() == id {
			bps = append(bps, bp)
		}
	}
	return bps
}

func (d *Debugger) FunctionArguments() ([]*proc.Variable, error) {
	s, err := proc.ConvertEvalScope(d.Target(), -1, d.StackFrame, 0)
	if err != nil {
		return nil, err
	}

	return s.FunctionArguments(ProcLoadConfig)
}

func (d *Debugger) LocalVariables() ([]*proc.Variable, error) {
	s, err := proc.ConvertEvalScope(d.Target(), -1, d.StackFrame, 0)
	if err != nil {
		return nil, err
	}

	return s.LocalVariables(ProcLoadConfig)
}

func (d *Debugger) PackageVariables() ([]*proc.Variable, error) {
	s, err := proc.ConvertEvalScope(d.Target(), -1, d.StackFrame, 0)
	if err != nil {
		return nil, err
	}

	return s.PackageVariables(ProcLoadConfig)
}

func (d *Debugger) RunningLines() []int {
	lines := []int{}

	if !d.TryLockTarget() {
		return lines
	}
	defer d.UnlockTarget()

	if ok, _ := d.Target().Valid(); ok {
		threads := d.Target().ThreadList()
		for _, thread := range threads {
			loc, err := thread.Location()
			if err == nil && loc.File == d.File {
				lines = append(lines, loc.Line)
			}
		}
	}

	return lines
}

func (d *Debugger) CreateWatch(expr string) {
	d.WatchesExpr = append(d.WatchesExpr, expr)
	SaveSession(ToSession(d))
}

func (d *Debugger) DeleteWatch(expr string) {
	nWatches := []string{}
	for _, v := range d.WatchesExpr {
		if v != expr {
			nWatches = append(nWatches, v)
		}
	}
	d.WatchesExpr = nWatches
}
