package info

import (
	"fmt"

	"github.com/zyedidia/micro/internal/buffer"
)

// The InfoBuf displays messages and other info at the bottom of the screen.
// It is respresented as a buffer and a message with a style.
type InfoBuf struct {
	*buffer.Buffer

	HasPrompt  bool
	HasMessage bool
	HasError   bool
	HasYN      bool

	PromptType string

	Msg    string
	YNResp bool
	Secret []rune

	// This map stores the history for all the different kinds of uses Prompt has
	// It's a map of history type -> history array
	History    map[string][]string
	HistoryNum int

	// Is the current message a message from the gutter
	HasGutter bool

	PromptCallback func(resp string, canceled bool)
	EventCallback  func(resp string)
	YNCallback     func(yes bool, canceled bool)
}

// NewBuffer returns a new infobuffer
func NewBuffer() *InfoBuf {
	ib := new(InfoBuf)
	ib.History = make(map[string][]string)

	ib.Buffer = buffer.NewBufferFromString("", "", buffer.BTInfo)
	ib.LoadHistory()

	return ib
}

// Close performs any cleanup necessary when shutting down the infobuffer
func (i *InfoBuf) Close() {
	i.SaveHistory()
}

// Message sends a message to the user
func (i *InfoBuf) Message(msg ...interface{}) {
	// only display a new message if there isn't an active prompt
	// this is to prevent overwriting an existing prompt to the user
	if i.HasPrompt == false {
		displayMessage := fmt.Sprint(msg...)
		// if there is no active prompt then style and display the message as normal
		i.Msg = displayMessage
		i.HasMessage, i.HasError = true, false
	}
}

// GutterMessage displays a message and marks it as a gutter message
func (i *InfoBuf) GutterMessage(msg ...interface{}) {
	i.Message(msg...)
	i.HasGutter = true
}

// ClearGutter clears the info bar and unmarks the message
func (i *InfoBuf) ClearGutter() {
	i.HasGutter = false
	i.Message("")
}

// Error sends an error message to the user
func (i *InfoBuf) Error(msg ...interface{}) {
	// only display a new message if there isn't an active prompt
	// this is to prevent overwriting an existing prompt to the user
	if i.HasPrompt == false {
		// if there is no active prompt then style and display the message as normal
		i.Msg = fmt.Sprint(msg...)
		i.HasMessage, i.HasError = false, true
	}
	// TODO: add to log?
}

// Prompt starts a prompt for the user, it takes a prompt, a possibly partially filled in msg
// and callbacks executed when the user executes an event and when the user finishes the prompt
// The eventcb passes the current user response as the argument and donecb passes the user's message
// and a boolean indicating if the prompt was canceled
func (i *InfoBuf) Prompt(prompt string, msg string, ptype string, eventcb func(string), donecb func(string, bool)) {
	// If we get another prompt mid-prompt we cancel the one getting overwritten
	if i.HasPrompt {
		i.DonePrompt(true)
	}

	if _, ok := i.History[ptype]; !ok {
		i.History[ptype] = []string{""}
	} else {
		i.History[ptype] = append(i.History[ptype], "")
	}
	i.HistoryNum = len(i.History[ptype]) - 1

	i.PromptType = ptype
	i.Msg = prompt
	i.HasPrompt = true
	i.HasMessage, i.HasError, i.HasYN = false, false, false
	i.Secret = []rune{}
	i.HasGutter = false
	i.PromptCallback = donecb
	i.EventCallback = eventcb
	i.Buffer.Insert(i.Buffer.Start(), msg)
}

// PasswordPrompt asks the user for a password and returns the result
func (i *InfoBuf) PasswordPrompt(verify bool, callback func(password string, canceled bool)) {
	eventcb := func(password string) {

	}
	passwordPrompt := func(prompt string, next func(password string, canceled bool)) {
		donecb := func(password string, canceled bool) {
			if canceled {
				callback("", true)
			} else if next != nil {
				next(password, canceled)
			}
		}
		i.Prompt(prompt, "", "secret", eventcb, donecb)
	}

	if verify {
		verifyPassword := ""
		next1 := func(password string, canceled bool) {
			if canceled {
				callback("", true)
			} else if password == verifyPassword {
				callback(password, canceled)
			} else {
				i.PasswordPrompt(verify, callback)
			}
		}
		next := func(password string, canceled bool) {
			verifyPassword = password
			passwordPrompt("Verify Password: ", next1)
		}
		passwordPrompt("Password: ", next)
		return
	}

	passwordPrompt("Password: ", callback)
	return
}

// YNPrompt creates a yes or no prompt, and the callback returns the yes/no result and whether
// the prompt was canceled
func (i *InfoBuf) YNPrompt(prompt string, donecb func(bool, bool)) {
	if i.HasPrompt {
		i.DonePrompt(true)
	}

	i.Msg = prompt
	i.HasPrompt = true
	i.HasYN = true
	i.HasMessage, i.HasError = false, false
	i.HasGutter = false
	i.YNCallback = donecb
}

// DonePrompt finishes the current prompt and indicates whether or not it was canceled
func (i *InfoBuf) DonePrompt(canceled bool) {
	hadYN := i.HasYN
	i.HasPrompt = false
	i.HasYN = false
	i.HasGutter = false
	if !hadYN {
		if i.PromptCallback != nil {
			callback := i.PromptCallback
			i.PromptCallback = nil
			if canceled {
				h := i.History[i.PromptType]
				i.History[i.PromptType] = h[:len(h)-1]
				callback("", true)
			} else {
				if i.PromptType == "secret" {
					secret := string(i.Secret)
					i.Secret = []rune{}
					callback(secret, false)
				} else {
					resp := string(i.LineBytes(0))
					h := i.History[i.PromptType]
					h[len(h)-1] = resp
					callback(resp, false)
				}
			}
		}
		i.Replace(i.Start(), i.End(), "")
	}
	if i.YNCallback != nil && hadYN {
		i.YNCallback(i.YNResp, canceled)
	}
}

// Reset resets the infobuffer's msg and info
func (i *InfoBuf) Reset() {
	i.Msg = ""
	i.HasPrompt, i.HasMessage, i.HasError = false, false, false
	i.HasGutter = false
}