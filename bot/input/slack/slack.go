package slack

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/micro/cli"
	"github.com/micro/micro/bot/command"
	"github.com/micro/micro/bot/input"
	"github.com/nlopes/slack"
)

type slackInput struct {
	debug bool
	token string

	sync.Mutex
	running bool
	exit    chan bool

	api *slack.Client

	ctx  sync.RWMutex
	cmds map[string]command.Command
}

func init() {
	input.Inputs["slack"] = NewInput()
}

func (s *slackInput) exec(ev *slack.MessageEvent, rtm *slack.RTM, args []string, name string) {
	s.ctx.RLock()
	defer s.ctx.RUnlock()

	// no args, bail out
	if len(args) == 0 {
		return
	}

	for _, cmd := range s.cmds {
		if args[0] != cmd.Name() {
			continue
		}

		rsp, err := cmd.Exec(args...)
		if err != nil {
			text := fmt.Sprintf("@%s: error executing command: %v", name, err)
			rtm.SendMessage(rtm.NewOutgoingMessage(text, ev.Channel))
			return
		}

		text := fmt.Sprintf("@%s: %s", name, string(rsp))

		if len(name) == 0 || strings.HasPrefix(ev.Channel, "D") {
			text = string(rsp)
		}

		rtm.SendMessage(rtm.NewOutgoingMessage(text, ev.Channel))
	}
}

func (s *slackInput) process(ev *slack.MessageEvent, rtm *slack.RTM, auth *slack.AuthTestResponse, name string) {
	if ev.Type != "message" {
		return
	}

	if len(ev.Text) == 0 {
		return
	}

	// don't process self
	if ev.User == auth.User {
		return
	}

	// only process the following
	switch {
	case strings.HasPrefix(ev.Channel, "D"):
	case strings.HasPrefix(ev.Text, auth.User):
	case strings.HasPrefix(ev.Text, fmt.Sprintf("<@%s>", auth.UserID)):
	default:
		return
	}

	var args []string

	// setup the args
	switch {
	case strings.HasPrefix(ev.Text, auth.User):
		args = strings.Split(ev.Text, " ")[1:]
	case strings.HasPrefix(ev.Text, fmt.Sprintf("<@%s>", auth.UserID)):
		args = strings.Split(ev.Text, " ")[1:]
	default:
		args = strings.Split(ev.Text, " ")
	}

	s.exec(ev, rtm, args, name)
}

func (p *slackInput) run(auth *slack.AuthTestResponse) {
	rtm := p.api.NewRTM()
	go rtm.ManageConnection()
	defer rtm.Disconnect()

	// func retrieves user names and maps to IDs
	fn := func() map[string]string {
		names := make(map[string]string)
		users, err := rtm.Client.GetUsers()
		if err != nil {
			return names
		}

		for _, user := range users {
			names[user.ID] = user.Name
		}

		return names
	}

	// get names
	names := fn()

	// update names ticker
	t := time.NewTicker(time.Minute)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			names = fn()
		case <-p.exit:
			return
		case e := <-rtm.IncomingEvents:
			switch ev := e.Data.(type) {
			case *slack.MessageEvent:
				p.process(ev, rtm, auth, names[ev.User])
			case *slack.InvalidAuthEvent:
				return
			}
		}
	}
}

func (p *slackInput) Flags() []cli.Flag {
	return []cli.Flag{
		cli.BoolFlag{
			Name:  "slack_debug",
			Usage: "Slack debug output",
		},
		cli.StringFlag{
			Name:  "slack_token",
			Usage: "Slack token",
		},
	}
}

func (p *slackInput) Init(ctx *cli.Context) error {
	debug := ctx.Bool("slack_debug")
	token := ctx.String("slack_token")

	if len(token) == 0 {
		return errors.New("missing slack token")
	}

	p.debug = debug
	p.token = token

	return nil
}

func (p *slackInput) Connect() (input.Conn, error) {
	p.Lock()
	defer p.Unlock()

	if !p.running {
		return nil, errors.New("not running")
	}

	// test auth
	auth, err := p.api.AuthTest()
	if err != nil {
		return nil, err
	}

	rtm := p.api.NewRTM()
	exit := make(chan bool)

	go rtm.ManageConnection()

	go func() {
		select {
		case <-p.exit:
			select {
			case <-exit:
				return
			default:
				close(exit)
			}
		case <-exit:
		}

		rtm.Disconnect()
	}()

	return &slackConn{
		auth: auth,
		rtm:  rtm,
		exit: exit,
	}, nil
}

func (p *slackInput) Process(cmd command.Command) error {
	p.ctx.Lock()
	defer p.ctx.Unlock()

	if _, ok := p.cmds[cmd.Name()]; ok {
		return errors.New("Command with name " + cmd.Name() + " already exists")
	}

	p.cmds[cmd.Name()] = cmd
	return nil
}

func (p *slackInput) Start() error {
	if len(p.token) == 0 {
		return errors.New("missing slack token")
	}

	p.Lock()
	defer p.Unlock()

	if p.running {
		return nil
	}

	api := slack.New(p.token)
	api.SetDebug(p.debug)

	// test auth
	auth, err := api.AuthTest()
	if err != nil {
		return err
	}

	p.api = api
	p.exit = make(chan bool)
	p.running = true
	go p.run(auth)

	return nil
}

func (p *slackInput) Stop() error {
	p.Lock()
	defer p.Unlock()

	if !p.running {
		return nil
	}

	close(p.exit)
	p.running = false
	return nil
}

func (p *slackInput) String() string {
	return "slack"
}

func NewInput() input.Input {
	return &slackInput{
		cmds: make(map[string]command.Command),
	}
}
