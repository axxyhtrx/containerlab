package config

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

type SshSession struct {
	In      io.Reader
	Out     io.WriteCloser
	Session *ssh.Session
}

// Display the SSH login message
var LoginMessages bool

// The reply the execute command and the prompt.
type SshReply struct{ result, prompt string }

// SshTransport setting needs to be set before calling Connect()
// SshTransport implement the Transport interface
type SshTransport struct {
	// Channel used to read. Can use Expect to Write & read wit timeout
	in chan SshReply
	// SSH Session
	ses *SshSession
	// Contains the first read after connecting
	LoginMessage SshReply
	// SSH parameters used in connect
	// defualt: 22
	Port int
	// extra debug print
	debug bool

	// SSH Options
	// required!
	SshConfig *ssh.ClientConfig

	// Character to split the incoming stream (#/$/>)
	// default: #
	PromptChar string

	// Kind specific transactions & prompt checking function
	K SshKind
}

// Creates the channel reading the SSH connection
//
// The first prompt is saved in LoginMessages
//
// - The channel read the SSH session, splits on PromptChar
// - Uses SshKind's PromptParse to split the received data in *result* and *prompt* parts
//   (if no valid prompt was found, prompt will simply be empty and result contain all the data)
// - Emit data
func (t *SshTransport) InChannel() {
	// Ensure we have a working channel
	t.in = make(chan SshReply)

	// setup a buffered string channel
	go func() {
		buf := make([]byte, 1024)
		tmpS := ""
		n, err := t.ses.In.Read(buf) //this reads the ssh terminal
		if err == nil {
			tmpS = string(buf[:n])
		}
		for err == nil {

			if strings.Contains(tmpS, "#") {
				parts := strings.Split(tmpS, "#")
				li := len(parts) - 1
				for i := 0; i < li; i++ {
					t.in <- *t.K.PromptParse(t, &parts[i])
				}
				tmpS = parts[li]
			}
			n, err = t.ses.In.Read(buf)
			tmpS += string(buf[:n])
		}
		log.Debugf("In Channel closing: %v", err)
		t.in <- SshReply{
			result: tmpS,
			prompt: "",
		}
	}()

	// Save first prompt
	t.LoginMessage = t.Run("", 15)
	if LoginMessages {
		t.LoginMessage.Infof("")
	}
}

// Run a single command and wait for the reply
func (t *SshTransport) Run(command string, timeout int) SshReply {
	if command != "" {
		t.ses.Writeln(command)
	}

	sHistory := ""

	for {
		// Read from the channel with a timeout
		var rr string

		select {
		case <-time.After(time.Duration(timeout) * time.Second):
			log.Warnf("timeout waiting for prompt: %s", command)
			return SshReply{}
		case ret := <-t.in:
			if t.debug {
				ret.Debug()
			}

			if ret.prompt == "" && ret.result != "" {
				// we should continue reading...
				sHistory += ret.result
				timeout = 1 // reduce timeout, node is already sending data
				continue
			}
			if ret.result == "" && ret.prompt == "" {
				log.Errorf("received zero?")
				continue
			}

			if sHistory == "" {
				rr = strings.Trim(ret.result, " \n\r\t")
			} else {
				rr = strings.Trim(sHistory+ret.result, " \n\r\t")
				sHistory = ""
			}

			if strings.HasPrefix(rr, command) {
				rr = strings.Trim(rr[len(command):], " \n\r\t")
			} else if !strings.Contains(rr, command) {
				sHistory = rr
				continue
			}
			res := SshReply{
				result: rr,
				prompt: ret.prompt,
			}
			res.Debug()
			return res
		}
	}
}

// Write a config snippet (a set of commands)
// Session NEEDS to be configurable for other kinds
// Part of the Transport interface
func (t *SshTransport) Write(snip *ConfigSnippet) error {
	if len(snip.Data) == 0 {
		return nil
	}

	transaction := !strings.HasPrefix(snip.templateName, "show-")

	err := t.K.ConfigStart(t, snip.TargetNode.ShortName, transaction)
	if err != nil {
		return err
	}

	c, b := 0, 0
	var r SshReply

	for _, l := range snip.Lines() {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		c += 1
		b += len(l)
		r = t.Run(l, 5)
		if r.result != "" {
			r.Infof(snip.TargetNode.ShortName)
		}
	}

	if transaction {
		commit, _ := t.K.ConfigCommit(t)

		commit.Infof("COMMIT %s - %d lines %d bytes", snip, c, b)
	}

	return nil
}

// Connect to a host
// Part of the Transport interface
func (t *SshTransport) Connect(host string) error {
	// Assign Default Values
	if t.PromptChar == "" {
		t.PromptChar = "#"
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if t.SshConfig == nil {
		return fmt.Errorf("require auth credentials in SshConfig")
	}

	// Start some client config
	host = fmt.Sprintf("%s:%d", host, t.Port)
	//sshConfig := &ssh.ClientConfig{}
	//SshConfigWithUserNamePassword(sshConfig, "admin", "admin")

	ses_, err := NewSshSession(host, t.SshConfig)
	if err != nil || ses_ == nil {
		return fmt.Errorf("cannot connect to %s: %s", host, err)
	}
	t.ses = ses_

	log.Infof("Connected to %s\n", host)
	t.InChannel()
	//Read to first prompt
	return nil
}

// Close the Session and channels
// Part of the Transport interface
func (t *SshTransport) Close() {
	if t.in != nil {
		close(t.in)
		t.in = nil
	}
	t.ses.Close()
}

// Add a basic username & password to a config.
// Will initilize the config if required
func SshConfigWithUserNamePassword(config *ssh.ClientConfig, username, password string) {
	if config == nil {
		config = &ssh.ClientConfig{}
	}
	config.User = username
	if config.Auth == nil {
		config.Auth = []ssh.AuthMethod{}
	}
	config.Auth = append(config.Auth, ssh.Password(password))
	config.HostKeyCallback = ssh.InsecureIgnoreHostKey()
}

// Create a new SSH session (Dial, open in/out pipes and start the shell)
// pass the authntication details in sshConfig
func NewSshSession(host string, sshConfig *ssh.ClientConfig) (*SshSession, error) {
	if !strings.Contains(host, ":") {
		return nil, fmt.Errorf("include the port in the host: %s", host)
	}

	connection, err := ssh.Dial("tcp", host, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %s", err)
	}
	session, err := connection.NewSession()
	if err != nil {
		return nil, err
	}
	sshIn, err := session.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("session stdout: %s", err)
	}
	sshOut, err := session.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("session stdin: %s", err)
	}
	// sshIn2, err := session.StderrPipe()
	// if err != nil {
	// 	return nil, fmt.Errorf("session stderr: %s", err)
	// }
	// Request PTY (required for srl)
	modes := ssh.TerminalModes{
		ssh.ECHO: 1, // disable echo
	}
	err = session.RequestPty("dumb", 24, 100, modes)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("pty request failed: %s", err)
	}

	if err := session.Shell(); err != nil {
		session.Close()
		return nil, fmt.Errorf("session shell: %s", err)
	}

	return &SshSession{
		Session: session,
		In:      sshIn,
		Out:     sshOut,
	}, nil
}

func (ses *SshSession) Writeln(command string) (int, error) {
	return ses.Out.Write([]byte(command + "\r"))
}

func (ses *SshSession) Close() {
	log.Debugf("Closing session")
	ses.Session.Close()
}

// This is a helper funciton to parse the prompt, and can be used by SshKind's ParsePrompt
// Used in SROS & SRL today
func promptParseNoSpaces(in *string, promptChar string, lines int) *SshReply {
	n := strings.LastIndex(*in, "\n")
	if n < 0 {
		return &SshReply{
			result: *in,
			prompt: "",
		}

	}
	if strings.Contains((*in)[n:], " ") {
		return &SshReply{
			result: *in,
			prompt: "",
		}
	}
	if lines > 1 {
		// Add another line to the prompt
		res := (*in)[:n]
		n = strings.LastIndex(res, "\n")
	}
	if n < 0 {
		n = 0
	}
	return &SshReply{
		result: (*in)[:n],
		prompt: (*in)[n:] + promptChar,
	}
}

// an interface to implement kind specific methods for transactions and prompt checking
type SshKind interface {
	// Start a config transaction
	ConfigStart(s *SshTransport, node string, transaction bool) error
	// Commit a config transaction
	ConfigCommit(s *SshTransport) (SshReply, error)
	// Prompt parsing function.
	// This function receives string, split by the delimiter and should ensure this is a valid prompt
	// Valid prompt, strip te prompt from the result and add it to the prompt in SshReply
	//
	// A defualt implementation is promptParseNoSpaces, which simply ensures there are
	// no spaces between the start of the line and the #
	PromptParse(s *SshTransport, in *string) *SshReply
}

// implements SShKind
type VrSrosSshKind struct{}

func (sk *VrSrosSshKind) ConfigStart(s *SshTransport, node string, transaction bool) error {
	s.PromptChar = "#" // ensure it's '#'
	//s.debug = true
	if transaction {
		cc := s.Run("/configure global", 5)
		if cc.result != "" {
			cc.Infof(node)
		}
		cc = s.Run("discard", 1)
		if cc.result != "" {
			cc.Infof("%s discard", node)
		}
	} else {
		s.Run("/environment more false", 5)
	}
	return nil
}
func (sk *VrSrosSshKind) ConfigCommit(s *SshTransport) (SshReply, error) {
	return s.Run("commit", 10), nil
}
func (sk *VrSrosSshKind) PromptParse(s *SshTransport, in *string) *SshReply {
	return promptParseNoSpaces(in, s.PromptChar, 2)
}

// implements SShKind
type SrlSshKind struct{}

func (sk *SrlSshKind) ConfigStart(s *SshTransport, node string, transaction bool) error {
	s.PromptChar = "#" // ensure it's '#'
	s.debug = true
	if transaction {
		s.Run("enter candidate", 5)
		s.Run("discard stay", 2)
	}
	return nil
}
func (sk *SrlSshKind) ConfigCommit(s *SshTransport) (SshReply, error) {
	return s.Run("commit now", 10), nil
}
func (sk *SrlSshKind) PromptParse(s *SshTransport, in *string) *SshReply {
	return promptParseNoSpaces(in, s.PromptChar, 2)
}

func (r *SshReply) Debug() {
	_, fn, line, _ := runtime.Caller(1)
	log.Debugf("(%s line %d) *RESULT: %s.\n | %v\n*PROMPT:%v.\n*PROMPT:%v.\n", fn, line, r.result, []byte(r.result), r.prompt, []byte(r.prompt))
}

func (r *SshReply) Infof(msg string, args ...interface{}) {
	var s string
	if r.result != "" {
		s = "\n  | "
		s += strings.Join(strings.Split(r.result, "\n"), s)
	}
	log.Infof(msg+s, args...)
}
