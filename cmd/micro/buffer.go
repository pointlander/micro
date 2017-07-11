package main

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"

	"github.com/mitchellh/go-homedir"
	"github.com/zyedidia/micro/cmd/micro/highlight"
)

// Buffer stores the text for files that are loaded into the text editor
// It uses a rope to efficiently store the string and contains some
// simple functions for saving and wrapper functions for modifying the rope
type Buffer struct {
	// The eventhandler for undo/redo
	*EventHandler
	// This stores all the text in the buffer as an array of lines
	*LineArray

	Cursor    Cursor
	cursors   []*Cursor // for multiple cursors
	curCursor int       // the current cursor

	// Path to the file on disk
	Path string
	// Absolute path to the file on disk
	AbsPath string
	// Name of the buffer on the status line
	name string

	// Whether or not the buffer has been modified since it was opened
	IsModified bool

	// Stores the last modification time of the file the buffer is pointing to
	ModTime time.Time

	NumLines int

	syntaxDef   *highlight.Def
	highlighter *highlight.Highlighter

	// Buffer local settings
	Settings map[string]interface{}

	Password string
}

// The SerializedBuffer holds the types that get serialized when a buffer is saved
// These are used for the savecursor and saveundo options
type SerializedBuffer struct {
	EventHandler *EventHandler
	Cursor       Cursor
	ModTime      time.Time
}

// NewBufferFromString creates a new buffer from a given string with a given path
func NewBufferFromString(text, path string) *Buffer {
	return NewBufferWithPassword(strings.NewReader(text), int64(len(text)), path, "")
}

// NewBuffer creates a new buffer from a given reader with a given path
func NewBuffer(reader io.Reader, size int64, path string) *Buffer {
	return NewBufferWithPassword(reader, size, path, "")
}

// NewBufferFromStringWithPassword creates a new buffer from a given string with a given path
// and password
func NewBufferFromStringWithPassword(text, path, password string) *Buffer {
	return NewBufferWithPassword(strings.NewReader(text), int64(len(text)), path, password)
}

// NewBufferWithPassword creates a new buffer from a given reader with a given path
// and password
func NewBufferWithPassword(reader io.Reader, size int64, path, password string) *Buffer {
	if path != "" {
		for _, tab := range tabs {
			for _, view := range tab.views {
				if view.Buf.Path == path {
					return view.Buf
				}
			}
		}
	}

	b := new(Buffer)
	var err error
	reader, err = b.Decode(reader, path, password)
	if err != nil {
		return NewBufferFromString(fmt.Sprintf("%s: %v", path, err), "")
	}
	b.LineArray = NewLineArray(size, reader)

	b.Settings = DefaultLocalSettings()
	for k, v := range globalSettings {
		if _, ok := b.Settings[k]; ok {
			b.Settings[k] = v
		}
	}

	absPath, _ := filepath.Abs(path)

	b.Path = path
	b.AbsPath = absPath

	// The last time this file was modified
	b.ModTime, _ = GetModTime(b.Path)

	b.EventHandler = NewEventHandler(b)

	b.Update()
	b.UpdateRules()

	if _, err := os.Stat(configDir + "/buffers/"); os.IsNotExist(err) {
		os.Mkdir(configDir+"/buffers/", os.ModePerm)
	}

	// Put the cursor at the first spot
	cursorStartX := 0
	cursorStartY := 0
	// If -startpos LINE,COL was passed, use start position LINE,COL
	if len(*flagStartPos) > 0 {
		positions := strings.Split(*flagStartPos, ",")
		if len(positions) == 2 {
			lineNum, errPos1 := strconv.Atoi(positions[0])
			colNum, errPos2 := strconv.Atoi(positions[1])
			if errPos1 == nil && errPos2 == nil {
				cursorStartX = colNum
				cursorStartY = lineNum - 1
				// Check to avoid line overflow
				if cursorStartY > b.NumLines {
					cursorStartY = b.NumLines - 1
				} else if cursorStartY < 0 {
					cursorStartY = 0
				}
				// Check to avoid column overflow
				if cursorStartX > len(b.Line(cursorStartY)) {
					cursorStartX = len(b.Line(cursorStartY))
				} else if cursorStartX < 0 {
					cursorStartX = 0
				}
			}
		}
	}
	b.Cursor = Cursor{
		Loc: Loc{
			X: cursorStartX,
			Y: cursorStartY,
		},
		buf: b,
	}

	InitLocalSettings(b)

	if b.Settings["savecursor"].(bool) || b.Settings["saveundo"].(bool) {
		// If either savecursor or saveundo is turned on, we need to load the serialized information
		// from ~/.config/micro/buffers
		file, err := os.Open(configDir + "/buffers/" + EscapePath(b.AbsPath))
		if err == nil {
			var buffer SerializedBuffer
			decoder := gob.NewDecoder(file)
			gob.Register(TextEvent{})
			err = decoder.Decode(&buffer)
			if err != nil {
				TermMessage(err.Error(), "\n", "You may want to remove the files in ~/.config/micro/buffers (these files store the information for the 'saveundo' and 'savecursor' options) if this problem persists.")
			}
			if b.Settings["savecursor"].(bool) {
				b.Cursor = buffer.Cursor
				b.Cursor.buf = b
				b.Cursor.Relocate()
			}

			if b.Settings["saveundo"].(bool) {
				// We should only use last time's eventhandler if the file wasn't by someone else in the meantime
				if b.ModTime == buffer.ModTime {
					b.EventHandler = buffer.EventHandler
					b.EventHandler.buf = b
				}
			}
		}
		file.Close()
	}

	b.cursors = []*Cursor{&b.Cursor}

	b.Password = password

	return b
}

func (b *Buffer) GetName() string {
	if b.name == "" {
		if b.Path == "" {
			return "No name"
		}
		return b.Path
	}
	return b.name
}

// UpdateRules updates the syntax rules and filetype for this buffer
// This is called when the colorscheme changes
func (b *Buffer) UpdateRules() {
	rehighlight := false
	var files []*highlight.File
	for _, f := range ListRuntimeFiles(RTSyntax) {
		data, err := f.Data()
		if err != nil {
			TermMessage("Error loading syntax file " + f.Name() + ": " + err.Error())
		} else {
			file, err := highlight.ParseFile(data)
			if err != nil {
				TermMessage("Error loading syntax file " + f.Name() + ": " + err.Error())
				continue
			}
			ftdetect, err := highlight.ParseFtDetect(file)
			if err != nil {
				TermMessage("Error loading syntax file " + f.Name() + ": " + err.Error())
				continue
			}

			ft := b.Settings["filetype"].(string)
			if ft == "Unknown" || ft == "" {
				if highlight.MatchFiletype(ftdetect, b.Path, b.lines[0].data) {
					header := new(highlight.Header)
					header.FileType = file.FileType
					header.FtDetect = ftdetect
					b.syntaxDef, err = highlight.ParseDef(file, header)
					if err != nil {
						TermMessage("Error loading syntax file " + f.Name() + ": " + err.Error())
						continue
					}
					rehighlight = true
				}
			} else {
				if file.FileType == ft {
					header := new(highlight.Header)
					header.FileType = file.FileType
					header.FtDetect = ftdetect
					b.syntaxDef, err = highlight.ParseDef(file, header)
					if err != nil {
						TermMessage("Error loading syntax file " + f.Name() + ": " + err.Error())
						continue
					}
					rehighlight = true
				}
			}
			files = append(files, file)
		}
	}

	if b.syntaxDef != nil {
		highlight.ResolveIncludes(b.syntaxDef, files)
	}
	files = nil

	if b.highlighter == nil || rehighlight {
		if b.syntaxDef != nil {
			b.Settings["filetype"] = b.syntaxDef.FileType
			b.highlighter = highlight.NewHighlighter(b.syntaxDef)
			if b.Settings["syntax"].(bool) {
				b.highlighter.HighlightStates(b)
			}
		}
	}
}

// FileType returns the buffer's filetype
func (b *Buffer) FileType() string {
	return b.Settings["filetype"].(string)
}

// IndentString returns a string representing one level of indentation
func (b *Buffer) IndentString() string {
	if b.Settings["tabstospaces"].(bool) {
		return Spaces(int(b.Settings["tabsize"].(float64)))
	}
	return "\t"
}

// CheckModTime makes sure that the file this buffer points to hasn't been updated
// by an external program since it was last read
// If it has, we ask the user if they would like to reload the file
func (b *Buffer) CheckModTime() {
	modTime, ok := GetModTime(b.Path)
	if ok {
		if modTime != b.ModTime {
			choice, canceled := messenger.YesNoPrompt("The file has changed since it was last read. Reload file? (y,n)")
			messenger.Reset()
			messenger.Clear()
			if !choice || canceled {
				// Don't load new changes -- do nothing
				b.ModTime, _ = GetModTime(b.Path)
			} else {
				// Load new changes
				b.ReOpen()
			}
		}
	}
}

// ReOpen reloads the current buffer from disk
func (b *Buffer) ReOpen() {
	var reader io.Reader
	reader, err := os.Open(b.Path)
	if err != nil {
		messenger.Error(err.Error())
		return
	}
	reader, err = b.Decode(reader, b.Path, b.Password)
	if err != nil {
		messenger.Error(err.Error())
		return
	}
	data, err := ioutil.ReadAll(reader)
	txt := string(data)

	if err != nil {
		messenger.Error(err.Error())
		return
	}
	b.EventHandler.ApplyDiff(txt)

	b.ModTime, _ = GetModTime(b.Path)
	b.IsModified = false
	b.Update()
	b.Cursor.Relocate()
}

// Update fetches the string from the rope and updates the `text` and `lines` in the buffer
func (b *Buffer) Update() {
	b.NumLines = len(b.lines)
}

func (b *Buffer) MergeCursors() {
	var cursors []*Cursor
	for i := 0; i < len(b.cursors); i++ {
		c1 := b.cursors[i]
		if c1 != nil {
			for j := 0; j < len(b.cursors); j++ {
				c2 := b.cursors[j]
				if c2 != nil && i != j && c1.Loc == c2.Loc {
					b.cursors[j] = nil
				}
			}
			cursors = append(cursors, c1)
		}
	}

	b.cursors = cursors
}

func (b *Buffer) UpdateCursors() {
	for i, c := range b.cursors {
		c.Num = i
	}
}

// Save saves the buffer to its default path
func (b *Buffer) Save() error {
	return b.SaveAs(b.Path)
}

// SaveWithSudo saves the buffer to the default path with sudo
func (b *Buffer) SaveWithSudo() error {
	return b.SaveAsWithSudo(b.Path)
}

// Serialize serializes the buffer to configDir/buffers
func (b *Buffer) Serialize() error {
	if b.Settings["savecursor"].(bool) || b.Settings["saveundo"].(bool) {
		file, err := os.Create(configDir + "/buffers/" + EscapePath(b.AbsPath))
		if err == nil {
			enc := gob.NewEncoder(file)
			gob.Register(TextEvent{})
			err = enc.Encode(SerializedBuffer{
				b.EventHandler,
				b.Cursor,
				b.ModTime,
			})
		}
		file.Close()
		return err
	}
	return nil
}

// Decode decodes a stream for loading the buffer
func (b *Buffer) Decode(reader io.Reader, path, password string) (io.Reader, error) {
	if password != "" {
		if strings.HasSuffix(path, ExtensionArmorGPG) {
			unarmored, err := armor.Decode(reader)
			if err != nil {
				return nil, err
			}
			reader = unarmored.Body
		}

		attempts := 0
		md, err := openpgp.ReadMessage(reader, nil, func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
			if attempts > 0 {
				return []byte{}, errors.New("invalid password")
			}
			attempts++
			return []byte(password), nil
		}, nil)
		if err != nil {
			return nil, err
		}

		reader = md.UnverifiedBody
	}

	return reader, nil
}

// Encode encodes the buffer for writing to disk
func (b *Buffer) Encode() ([]byte, error) {
	if b.Password != "" && strings.HasSuffix(b.Path, ExtensionArmorGPG) {
		buffer := &bytes.Buffer{}
		w, err := armor.Encode(buffer, "PGP SIGNATURE", nil)
		if err != nil {
			return nil, err
		}

		plaintext, err := openpgp.SymmetricallyEncrypt(w, []byte(b.Password), nil, nil)
		if err != nil {
			return nil, err
		}
		_, err = plaintext.Write([]byte(b.String()))
		if err != nil {
			return nil, err
		}

		plaintext.Close()
		w.Close()

		return buffer.Bytes(), nil
	} else if b.Password != "" {
		buffer := &bytes.Buffer{}
		plaintext, err := openpgp.SymmetricallyEncrypt(buffer, []byte(b.Password), nil, nil)
		if err != nil {
			return nil, err
		}
		_, err = plaintext.Write([]byte(b.String()))
		if err != nil {
			return nil, err
		}

		plaintext.Close()

		return buffer.Bytes(), nil
	}

	return []byte(b.String()), nil
}

// SaveAs saves the buffer to a specified path (filename), creating the file if it does not exist
func (b *Buffer) SaveAs(filename string) error {
	b.UpdateRules()
	dir, _ := homedir.Dir()
	if b.Settings["rmtrailingws"].(bool) {
		r, _ := regexp.Compile(`[ \t]+$`)
		for lineNum, line := range b.Lines(0, b.NumLines) {
			indices := r.FindStringIndex(line)
			if indices == nil {
				continue
			}
			startLoc := Loc{indices[0], lineNum}
			b.deleteToEnd(startLoc)
		}
		b.Cursor.Relocate()
	}
	if b.Settings["eofnewline"].(bool) {
		end := b.End()
		if b.RuneAt(Loc{end.X - 1, end.Y}) != '\n' {
			b.Insert(end, "\n")
		}
	}
	data, err := b.Encode()
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filename, data, 0644)
	if err == nil {
		b.Path = strings.Replace(filename, "~", dir, 1)
		b.IsModified = false
		b.ModTime, _ = GetModTime(filename)
		return b.Serialize()
	}
	b.ModTime, _ = GetModTime(filename)
	return err
}

// SaveAsWithSudo is the same as SaveAs except it uses a neat trick
// with tee to use sudo so the user doesn't have to reopen micro with sudo
func (b *Buffer) SaveAsWithSudo(filename string) error {
	data, err := b.Encode()
	if err != nil {
		return err
	}

	b.UpdateRules()
	b.Path = filename

	// The user may have already used sudo in which case we won't need the password
	// It's a bit nicer for them if they don't have to enter the password every time
	_, err = RunShellCommand("sudo -v")
	needPassword := err != nil

	// If we need the password, we have to close the screen and ask using the shell
	if needPassword {
		// Shut down the screen because we're going to interact directly with the shell
		screen.Fini()
		screen = nil
	}

	// Set up everything for the command
	cmd := exec.Command("sudo", "tee", filename)
	cmd.Stdin = bytes.NewBuffer(data)

	// This is a trap for Ctrl-C so that it doesn't kill micro
	// Instead we trap Ctrl-C to kill the program we're running
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			cmd.Process.Kill()
		}
	}()

	// Start the command
	cmd.Start()
	err = cmd.Wait()

	// If we needed the password, we closed the screen, so we have to initialize it again
	if needPassword {
		// Start the screen back up
		InitScreen()
	}
	if err == nil {
		b.IsModified = false
		b.ModTime, _ = GetModTime(filename)
		b.Serialize()
	}
	return err
}

func (b *Buffer) insert(pos Loc, value []byte) {
	b.IsModified = true
	b.LineArray.insert(pos, value)
	b.Update()
}
func (b *Buffer) remove(start, end Loc) string {
	b.IsModified = true
	sub := b.LineArray.remove(start, end)
	b.Update()
	return sub
}
func (b *Buffer) deleteToEnd(start Loc) {
	b.IsModified = true
	b.LineArray.DeleteToEnd(start)
	b.Update()
}

// Start returns the location of the first character in the buffer
func (b *Buffer) Start() Loc {
	return Loc{0, 0}
}

// End returns the location of the last character in the buffer
func (b *Buffer) End() Loc {
	return Loc{utf8.RuneCount(b.lines[b.NumLines-1].data), b.NumLines - 1}
}

// RuneAt returns the rune at a given location in the buffer
func (b *Buffer) RuneAt(loc Loc) rune {
	line := []rune(b.Line(loc.Y))
	if len(line) > 0 {
		return line[loc.X]
	}
	return '\n'
}

// Line returns a single line
func (b *Buffer) Line(n int) string {
	if n >= len(b.lines) {
		return ""
	}
	return string(b.lines[n].data)
}

func (b *Buffer) LinesNum() int {
	return len(b.lines)
}

// Lines returns an array of strings containing the lines from start to end
func (b *Buffer) Lines(start, end int) []string {
	lines := b.lines[start:end]
	var slice []string
	for _, line := range lines {
		slice = append(slice, string(line.data))
	}
	return slice
}

// Len gives the length of the buffer
func (b *Buffer) Len() int {
	return Count(b.String())
}

// MoveLinesUp moves the range of lines up one row
func (b *Buffer) MoveLinesUp(start int, end int) {
	// 0 < start < end <= len(b.lines)
	if start < 1 || start >= end || end > len(b.lines) {
		return // what to do? FIXME
	}
	if end == len(b.lines) {
		b.Insert(
			Loc{
				utf8.RuneCount(b.lines[end-1].data),
				end - 1,
			},
			"\n"+b.Line(start-1),
		)
	} else {
		b.Insert(
			Loc{0, end},
			b.Line(start-1)+"\n",
		)
	}
	b.Remove(
		Loc{0, start - 1},
		Loc{0, start},
	)
}

// MoveLinesDown moves the range of lines down one row
func (b *Buffer) MoveLinesDown(start int, end int) {
	// 0 <= start < end < len(b.lines)
	// if end == len(b.lines), we can't do anything here because the
	// last line is unaccessible, FIXME
	if start < 0 || start >= end || end >= len(b.lines)-1 {
		return // what to do? FIXME
	}
	b.Insert(
		Loc{0, start},
		b.Line(end)+"\n",
	)
	end++
	b.Remove(
		Loc{0, end},
		Loc{0, end + 1},
	)
}

// ClearMatches clears all of the syntax highlighting for this buffer
func (b *Buffer) ClearMatches() {
	for i := range b.lines {
		b.SetMatch(i, nil)
		b.SetState(i, nil)
	}
}
