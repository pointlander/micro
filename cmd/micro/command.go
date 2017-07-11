package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	humanize "github.com/dustin/go-humanize"
	"github.com/mitchellh/go-homedir"
)

// A Command contains a action (a function to call) as well as information about how to autocomplete the command
type Command struct {
	action      func([]string)
	completions []Completion
}

// A StrCommand is similar to a command but keeps the name of the action
type StrCommand struct {
	action      string
	completions []Completion
}

var commands map[string]Command

var commandActions map[string]func([]string)

func init() {
	commandActions = map[string]func([]string){
		"Set":        Set,
		"SetLocal":   SetLocal,
		"Show":       Show,
		"Run":        Run,
		"Bind":       Bind,
		"Quit":       Quit,
		"Save":       Save,
		"Replace":    Replace,
		"ReplaceAll": ReplaceAll,
		"VSplit":     VSplit,
		"HSplit":     HSplit,
		"Tab":        NewTab,
		"Help":       Help,
		"Eval":       Eval,
		"ToggleLog":  ToggleLog,
		"Plugin":     PluginCmd,
		"Reload":     Reload,
		"Cd":         Cd,
		"Pwd":        Pwd,
		"Open":       Open,
		"TabSwitch":  TabSwitch,
		"MemUsage":   MemUsage,
	}
}

// InitCommands initializes the default commands
func InitCommands() {
	commands = make(map[string]Command)

	defaults := DefaultCommands()
	parseCommands(defaults)
}

func parseCommands(userCommands map[string]StrCommand) {
	for k, v := range userCommands {
		MakeCommand(k, v.action, v.completions...)
	}
}

// MakeCommand is a function to easily create new commands
// This can be called by plugins in Lua so that plugins can define their own commands
func MakeCommand(name, function string, completions ...Completion) {
	action := commandActions[function]
	if _, ok := commandActions[function]; !ok {
		// If the user seems to be binding a function that doesn't exist
		// We hope that it's a lua function that exists and bind it to that
		action = LuaFunctionCommand(function)
	}

	commands[name] = Command{action, completions}
}

// DefaultCommands returns a map containing micro's default commands
func DefaultCommands() map[string]StrCommand {
	return map[string]StrCommand{
		"set":        {"Set", []Completion{OptionCompletion, NoCompletion}},
		"setlocal":   {"SetLocal", []Completion{OptionCompletion, NoCompletion}},
		"show":       {"Show", []Completion{OptionCompletion, NoCompletion}},
		"bind":       {"Bind", []Completion{NoCompletion}},
		"run":        {"Run", []Completion{NoCompletion}},
		"quit":       {"Quit", []Completion{NoCompletion}},
		"save":       {"Save", []Completion{NoCompletion}},
		"replace":    {"Replace", []Completion{NoCompletion}},
		"replaceall": {"ReplaceAll", []Completion{NoCompletion}},
		"vsplit":     {"VSplit", []Completion{FileCompletion, NoCompletion}},
		"hsplit":     {"HSplit", []Completion{FileCompletion, NoCompletion}},
		"tab":        {"Tab", []Completion{FileCompletion, NoCompletion}},
		"help":       {"Help", []Completion{HelpCompletion, NoCompletion}},
		"eval":       {"Eval", []Completion{NoCompletion}},
		"log":        {"ToggleLog", []Completion{NoCompletion}},
		"plugin":     {"Plugin", []Completion{PluginCmdCompletion, PluginNameCompletion}},
		"reload":     {"Reload", []Completion{NoCompletion}},
		"cd":         {"Cd", []Completion{FileCompletion}},
		"pwd":        {"Pwd", []Completion{NoCompletion}},
		"open":       {"Open", []Completion{FileCompletion}},
		"tabswitch":  {"TabSwitch", []Completion{NoCompletion}},
		"memusage":   {"MemUsage", []Completion{NoCompletion}},
	}
}

// PluginCmd installs, removes, updates, lists, or searches for given plugins
func PluginCmd(args []string) {
	if len(args) >= 1 {
		switch args[0] {
		case "install":
			installedVersions := GetInstalledVersions(false)
			for _, plugin := range args[1:] {
				pp := GetAllPluginPackages().Get(plugin)
				if pp == nil {
					messenger.Error("Unknown plugin \"" + plugin + "\"")
				} else if err := pp.IsInstallable(); err != nil {
					messenger.Error("Error installing ", plugin, ": ", err)
				} else {
					for _, installed := range installedVersions {
						if pp.Name == installed.pack.Name {
							if pp.Versions[0].Version.Compare(installed.Version) == 1 {
								messenger.Error(pp.Name, " is already installed but out-of-date: use 'plugin update ", pp.Name, "' to update")
							} else {
								messenger.Error(pp.Name, " is already installed")
							}
						}
					}
					pp.Install()
				}
			}
		case "remove":
			removed := ""
			for _, plugin := range args[1:] {
				// check if the plugin exists.
				if _, ok := loadedPlugins[plugin]; ok {
					UninstallPlugin(plugin)
					removed += plugin + " "
					continue
				}
			}
			if !IsSpaces(removed) {
				messenger.Message("Removed ", removed)
			} else {
				messenger.Error("The requested plugins do not exist")
			}
		case "update":
			UpdatePlugins(args[1:])
		case "list":
			plugins := GetInstalledVersions(false)
			messenger.AddLog("----------------")
			messenger.AddLog("The following plugins are currently installed:\n")
			for _, p := range plugins {
				messenger.AddLog(fmt.Sprintf("%s (%s)", p.pack.Name, p.Version))
			}
			messenger.AddLog("----------------")
			if len(plugins) > 0 {
				if CurView().Type != vtLog {
					ToggleLog([]string{})
				}
			}
		case "search":
			plugins := SearchPlugin(args[1:])
			messenger.Message(len(plugins), " plugins found")
			for _, p := range plugins {
				messenger.AddLog("----------------")
				messenger.AddLog(p.String())
			}
			messenger.AddLog("----------------")
			if len(plugins) > 0 {
				if CurView().Type != vtLog {
					ToggleLog([]string{})
				}
			}
		case "available":
			packages := GetAllPluginPackages()
			messenger.AddLog("Available Plugins:")
			for _, pkg := range packages {
				messenger.AddLog(pkg.Name)
			}
			if CurView().Type != vtLog {
				ToggleLog([]string{})
			}
		}
	} else {
		messenger.Error("Not enough arguments")
	}
}

// TabSwitch switches to a given tab either by name or by number
func TabSwitch(args []string) {
	if len(args) > 0 {
		num, err := strconv.Atoi(args[0])
		if err != nil {
			// Check for tab with this name

			found := false
			for _, t := range tabs {
				v := t.views[t.CurView]
				if v.Buf.GetName() == args[0] {
					curTab = v.TabNum
					found = true
				}
			}
			if !found {
				messenger.Error("Could not find tab: ", err)
			}
		} else {
			num--
			if num >= 0 && num < len(tabs) {
				curTab = num
			} else {
				messenger.Error("Invalid tab index")
			}
		}
	}
}

// Cd changes the current working directory
func Cd(args []string) {
	if len(args) > 0 {
		home, _ := homedir.Dir()
		path := strings.Replace(args[0], "~", home, 1)
		os.Chdir(path)
		for _, tab := range tabs {
			for _, view := range tab.views {
				wd, _ := os.Getwd()
				view.Buf.Path, _ = MakeRelative(view.Buf.AbsPath, wd)
				if p, _ := filepath.Abs(view.Buf.Path); !strings.Contains(p, wd) {
					view.Buf.Path = view.Buf.AbsPath
				}
			}
		}
	}
}

// MemUsage prints micro's memory usage
// Alloc shows how many bytes are currently in use
// Sys shows how many bytes have been requested from the operating system
// NumGC shows how many times the GC has been run
// Note that Go commonly reserves more memory from the OS than is currently in-use/required
// Additionally, even if Go returns memory to the OS, the OS does not always claim it because
// there may be plenty of memory to spare
func MemUsage(args []string) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	messenger.Message(fmt.Sprintf("Alloc: %v, Sys: %v, NumGC: %v", humanize.Bytes(mem.Alloc), humanize.Bytes(mem.Sys), mem.NumGC))
}

// Pwd prints the current working directory
func Pwd(args []string) {
	wd, err := os.Getwd()
	if err != nil {
		messenger.Message(err.Error())
	} else {
		messenger.Message(wd)
	}
}

// Open opens a new buffer with a given filename
func Open(args []string) {
	if len(args) > 0 {
		filename := args[0]
		// the filename might or might not be quoted, so unquote first then join the strings.
		filename = strings.Join(SplitCommandArgs(filename), " ")

		CurView().Open(filename)
	} else {
		messenger.Error("No filename")
	}
}

// ToggleLog toggles the log view
func ToggleLog(args []string) {
	buffer := messenger.getBuffer()
	if CurView().Type != vtLog {
		CurView().HSplit(buffer)
		CurView().Type = vtLog
		RedrawAll()
		buffer.Cursor.Loc = buffer.Start()
		CurView().Relocate()
		buffer.Cursor.Loc = buffer.End()
		CurView().Relocate()
	} else {
		CurView().Quit(true)
	}
}

// Reload reloads all files (syntax files, colorschemes...)
func Reload(args []string) {
	LoadAll()
}

// Help tries to open the given help page in a horizontal split
func Help(args []string) {
	if len(args) < 1 {
		// Open the default help if the user just typed "> help"
		CurView().openHelp("help")
	} else {
		helpPage := args[0]
		if FindRuntimeFile(RTHelp, helpPage) != nil {
			CurView().openHelp(helpPage)
		} else {
			messenger.Error("Sorry, no help for ", helpPage)
		}
	}
}

// VSplit opens a vertical split with file given in the first argument
// If no file is given, it opens an empty buffer in a new split
func VSplit(args []string) {
	if len(args) == 0 {
		CurView().VSplit(NewBufferFromString("", ""))
	} else {
		filename := args[0]
		home, _ := homedir.Dir()
		filename = strings.Replace(filename, "~", home, 1)
		file, err := os.Open(filename)
		fileInfo, _ := os.Stat(filename)

		if err == nil && fileInfo.IsDir() {
			messenger.Error(filename, " is a directory")
			return
		}

		defer file.Close()

		var buf *Buffer
		if err != nil {
			// File does not exist -- create an empty buffer with that name
			buf = NewBufferFromString("", filename)
		} else {
			buf = NewBuffer(file, FSize(file), filename)
		}
		CurView().VSplit(buf)
	}
}

// HSplit opens a horizontal split with file given in the first argument
// If no file is given, it opens an empty buffer in a new split
func HSplit(args []string) {
	if len(args) == 0 {
		CurView().HSplit(NewBufferFromString("", ""))
	} else {
		filename := args[0]
		home, _ := homedir.Dir()
		filename = strings.Replace(filename, "~", home, 1)
		file, err := os.Open(filename)
		fileInfo, _ := os.Stat(filename)

		if err == nil && fileInfo.IsDir() {
			messenger.Error(filename, " is a directory")
			return
		}

		defer file.Close()

		var buf *Buffer
		if err != nil {
			// File does not exist -- create an empty buffer with that name
			buf = NewBufferFromString("", filename)
		} else {
			buf = NewBuffer(file, FSize(file), filename)
		}
		CurView().HSplit(buf)
	}
}

// Eval evaluates a lua expression
func Eval(args []string) {
	if len(args) >= 1 {
		err := L.DoString(args[0])
		if err != nil {
			messenger.Error(err)
		}
	} else {
		messenger.Error("Not enough arguments")
	}
}

// NewTab opens the given file in a new tab
func NewTab(args []string) {
	if len(args) == 0 {
		CurView().AddTab(true)
	} else {
		filename := args[0]
		home, _ := homedir.Dir()
		filename = strings.Replace(filename, "~", home, 1)
		file, err := os.Open(filename)
		fileInfo, _ := os.Stat(filename)

		if err == nil && fileInfo.IsDir() {
			messenger.Error(filename, " is a directory")
			return
		}

		defer file.Close()

		var buf *Buffer
		if err != nil {
			buf = NewBufferFromString("", filename)
		} else {
			buf = NewBuffer(file, FSize(file), filename)
		}

		tab := NewTabFromView(NewView(buf))
		tab.SetNum(len(tabs))
		tabs = append(tabs, tab)
		curTab = len(tabs) - 1
		if len(tabs) == 2 {
			for _, t := range tabs {
				for _, v := range t.views {
					v.ToggleTabbar()
				}
			}
		}
	}
}

// Set sets an option
func Set(args []string) {
	if len(args) < 2 {
		messenger.Error("Not enough arguments")
		return
	}

	option := args[0]
	value := args[1]

	SetOptionAndSettings(option, value)
}

// SetLocal sets an option local to the buffer
func SetLocal(args []string) {
	if len(args) < 2 {
		messenger.Error("Not enough arguments")
		return
	}

	option := args[0]
	value := args[1]

	err := SetLocalOption(option, value, CurView())
	if err != nil {
		messenger.Error(err.Error())
	}
}

// Show shows the value of the given option
func Show(args []string) {
	if len(args) < 1 {
		messenger.Error("Please provide an option to show")
		return
	}

	option := GetOption(args[0])

	if option == nil {
		messenger.Error(args[0], " is not a valid option")
		return
	}

	messenger.Message(option)
}

// Bind creates a new keybinding
func Bind(args []string) {
	if len(args) < 2 {
		messenger.Error("Not enough arguments")
		return
	}
	BindKey(args[0], args[1])
}

// Run runs a shell command in the background
func Run(args []string) {
	// Run a shell command in the background (openTerm is false)
	HandleShellCommand(JoinCommandArgs(args...), false, true)
}

// Quit closes the main view
func Quit(args []string) {
	// Close the main view
	CurView().Quit(true)
}

// Save saves the buffer in the main view
func Save(args []string) {
	if len(args) == 0 {
		// Save the main view
		CurView().Save(true)
	} else {
		CurView().Buf.SaveAs(args[0])
	}
}

// Replace runs search and replace
func Replace(args []string) {
	if len(args) < 2 || len(args) > 3 {
		// We need to find both a search and replace expression
		messenger.Error("Invalid replace statement: " + strings.Join(args, " "))
		return
	}

	allAtOnce := false
	if len(args) == 3 {
		// user added -a flag
		if args[2] == "-a" {
			allAtOnce = true
		} else {
			messenger.Error("Invalid replace flag: " + args[2])
			return
		}
	}

	search := string(args[0])
	replace := string(args[1])

	regex, err := regexp.Compile("(?m)" + search)
	if err != nil {
		// There was an error with the user's regex
		messenger.Error(err.Error())
		return
	}

	view := CurView()

	found := 0
	replaceAll := func() {
		var deltas []Delta
		deltaXOffset := Count(replace) - Count(search)
		for i := 0; i < view.Buf.LinesNum(); i++ {
			matches := regex.FindAllIndex(view.Buf.lines[i].data, -1)
			str := string(view.Buf.lines[i].data)

			if matches != nil {
				xOffset := 0
				for _, m := range matches {
					from := Loc{runePos(m[0], str) + xOffset, i}
					to := Loc{runePos(m[1], str) + xOffset, i}

					xOffset += deltaXOffset

					deltas = append(deltas, Delta{replace, from, to})
					found++
				}
			}
		}
		view.Buf.MultipleReplace(deltas)
	}

	if allAtOnce {
		replaceAll()
	} else {
		for {
			// The 'check' flag was used
			Search(search, view, true)
			if !view.Cursor.HasSelection() {
				break
			}
			view.Relocate()
			RedrawAll()
			choice, canceled := messenger.LetterPrompt("Perform replacement? (y,n,a)", 'y', 'n', 'a')
			if canceled {
				if view.Cursor.HasSelection() {
					view.Cursor.Loc = view.Cursor.CurSelection[0]
					view.Cursor.ResetSelection()
				}
				messenger.Reset()
				break
			} else if choice == 'a' {
				if view.Cursor.HasSelection() {
					view.Cursor.Loc = view.Cursor.CurSelection[0]
					view.Cursor.ResetSelection()
				}
				messenger.Reset()
				replaceAll()
				break
			} else if choice == 'y' {
				view.Cursor.DeleteSelection()
				view.Buf.Insert(view.Cursor.Loc, replace)
				view.Cursor.ResetSelection()
				messenger.Reset()
				found++
			}
			if view.Cursor.HasSelection() {
				searchStart = view.Cursor.CurSelection[1]
			} else {
				searchStart = view.Cursor.Loc
			}
		}
	}
	view.Cursor.Relocate()

	if found > 1 {
		messenger.Message("Replaced ", found, " occurrences of ", search)
	} else if found == 1 {
		messenger.Message("Replaced ", found, " occurrence of ", search)
	} else {
		messenger.Message("Nothing matched ", search)
	}
}

// ReplaceAll replaces search term all at once
func ReplaceAll(args []string) {
	// aliased to Replace command
	Replace(append(args, "-a"))
}

// RunShellCommand executes a shell command and returns the output/error
func RunShellCommand(input string) (string, error) {
	inputCmd := SplitCommandArgs(input)[0]
	args := SplitCommandArgs(input)[1:]

	cmd := exec.Command(inputCmd, args...)
	outputBytes := &bytes.Buffer{}
	cmd.Stdout = outputBytes
	cmd.Stderr = outputBytes
	cmd.Start()
	err := cmd.Wait() // wait for command to finish
	outstring := outputBytes.String()
	return outstring, err
}

// HandleShellCommand runs the shell command
// The openTerm argument specifies whether a terminal should be opened (for viewing output
// or interacting with stdin)
func HandleShellCommand(input string, openTerm bool, waitToFinish bool) string {
	inputCmd := SplitCommandArgs(input)[0]
	if !openTerm {
		// Simply run the command in the background and notify the user when it's done
		messenger.Message("Running...")
		go func() {
			output, err := RunShellCommand(input)
			totalLines := strings.Split(output, "\n")

			if len(totalLines) < 3 {
				if err == nil {
					messenger.Message(inputCmd, " exited without error")
				} else {
					messenger.Message(inputCmd, " exited with error: ", err, ": ", output)
				}
			} else {
				messenger.Message(output)
			}
			// We have to make sure to redraw
			RedrawAll()
		}()
	} else {
		// Shut down the screen because we're going to interact directly with the shell
		screen.Fini()
		screen = nil

		args := SplitCommandArgs(input)[1:]

		// Set up everything for the command
		var outputBuf bytes.Buffer
		cmd := exec.Command(inputCmd, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = io.MultiWriter(os.Stdout, &outputBuf)
		cmd.Stderr = os.Stderr

		// This is a trap for Ctrl-C so that it doesn't kill micro
		// Instead we trap Ctrl-C to kill the program we're running
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		go func() {
			for range c {
				cmd.Process.Kill()
			}
		}()

		cmd.Start()
		err := cmd.Wait()

		output := outputBuf.String()
		if err != nil {
			output = err.Error()
		}

		if waitToFinish {
			// This is just so we don't return right away and let the user press enter to return
			TermMessage("")
		}

		// Start the screen back up
		InitScreen()

		return output
	}
	return ""
}

// HandleCommand handles input from the user
func HandleCommand(input string) {
	args := SplitCommandArgs(input)
	inputCmd := args[0]

	if _, ok := commands[inputCmd]; !ok {
		messenger.Error("Unknown command ", inputCmd)
	} else {
		commands[inputCmd].action(args[1:])
	}
}
