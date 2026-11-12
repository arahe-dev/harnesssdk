package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	harness "github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// Many agents and people: parties, mailboxes, rooms and claims.

func (c *cli) join(args []string) (int, error) {
	fs := c.flags("join")
	as := fs.String("as", "", "your alias (lower-case), such as reviewer or ana")
	human := fs.Bool("human", false, "you are a person, not an agent")
	note := fs.String("note", "", "what you are working on, for peers")
	cwd := fs.String("cwd", "", "your working folder (default: current)")
	var rooms multi
	fs.Var(&rooms, "room", "a room to be in (repeatable)")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	if *as == "" {
		return harness.ExitUsage, usageErr("join needs --as <alias>")
	}
	dir, err := folder(*cwd)
	if err != nil {
		return harness.ExitUsage, err
	}
	kind := "agent"
	if *human {
		kind = "human"
	}
	p, err := c.home.Join(*as, kind, dir, *note, rooms)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: you are %s (%s)%s; pass --as %s (or set %s=%s) to speak as it, and check your inbox with: harness inbox --as %s --wait 5m\n",
		p.Alias, p.Kind, roomList(p.Rooms), p.Alias, harness.EnvAs, p.Alias, p.Alias)
	return 0, nil
}

func roomList(rooms []string) string {
	if len(rooms) == 0 {
		return ""
	}
	return " in room " + strings.Join(rooms, ", room ")
}

func (c *cli) leave(args []string) (int, error) {
	fs := c.flags("leave")
	as := fs.String("as", "", "your alias")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	if *as == "" {
		*as = os.Getenv(harness.EnvAs)
	}
	if *as == "" {
		return harness.ExitUsage, usageErr("leave needs --as <alias>")
	}
	if err := c.home.Leave(*as); err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: %s left; its claims are released\n", *as)
	return 0, nil
}

func (c *cli) peers(args []string) (int, error) {
	fs := c.flags("peers")
	room := fs.String("room", "", "only parties in this room")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	parties, err := c.home.Parties()
	if err != nil {
		return harness.ExitUsage, err
	}
	runs, err := c.home.Runs(200)
	if err != nil {
		return harness.ExitUsage, err
	}
	claims, _ := c.home.Claims()
	held := map[string][]string{}
	for _, cl := range claims {
		held[cl.Holder] = append(held[cl.Holder], cl.Path)
	}
	type peer struct {
		Address string   `json:"address"`
		Kind    string   `json:"kind"` // agent, human or run
		State   string   `json:"state"`
		Detail  string   `json:"detail,omitempty"`
		Cwd     string   `json:"cwd,omitempty"`
		Rooms   []string `json:"rooms,omitempty"`
		Claims  []string `json:"claims,omitempty"`
	}
	var out []peer
	for _, p := range parties {
		if *room != "" && !contains(p.Rooms, *room) {
			continue
		}
		state := "present"
		if p.Away() {
			state = "away"
		}
		out = append(out, peer{p.Alias, p.Kind, state, p.Note, p.Cwd, p.Rooms, held[p.Alias]})
	}
	if *room == "" {
		for _, r := range runs {
			n := len(r.Turns)
			if n == 0 || (r.Turns[n-1].Outcome != "queued" && r.Turns[n-1].Outcome != "running") {
				continue
			}
			detail := "profile " + r.Profile
			if r.Owner != "" {
				detail += ", started by " + r.Owner
			} else if r.Parent != "" {
				detail += ", started by run:" + r.Parent
			}
			out = append(out, peer{"run:" + r.ID, "run", r.Turns[n-1].Outcome, detail, r.AgentDir(), nil, held["run:"+r.ID]})
		}
	}
	if *asJSON {
		if out == nil {
			out = []peer{}
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
		return 0, nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ADDRESS\tKIND\tSTATE\tDETAIL\tCLAIMS\tFOLDER")
	for _, p := range out {
		detail := p.Detail
		if len(p.Rooms) > 0 {
			detail = strings.TrimPrefix(detail+"; rooms "+strings.Join(p.Rooms, ","), "; ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Address, p.Kind, p.State, or(detail, "-"), dash(p.Claims), p.Cwd)
	}
	return 0, w.Flush()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (c *cli) post(args []string) (int, error) {
	fs := c.flags("post")
	as := fs.String("as", "", "who you are (an alias that joined); inside a run, the run")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) < 2 {
		return harness.ExitUsage, usageErr("post needs <to> \"text\": an alias, run:<id> or room:<name>")
	}
	from, err := c.home.Speaker(*as)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	id, err := c.home.Post(from, pos[0], strings.Join(pos[1:], " "))
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: posted %s to %s\n", id, pos[0])
	return 0, nil
}

func (c *cli) ask(args []string) (int, error) {
	fs := c.flags("ask")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the answer (exit 7 if none came)")
	to := fs.String("to", "", "who to ask: an alias, run:<id> or room:<name> (default, inside a run: whoever started it)")
	as := fs.String("as", "", "who is asking (an alias that joined); inside a run, the run")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) == 0 {
		return harness.ExitUsage, usageErr("ask needs a question")
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	question := strings.Join(pos, " ")
	var id, answer string
	var answered bool
	if *to == "" && *as == "" {
		_, id, answer, answered, err = r.Ask(ctx, question)
	} else {
		var from string
		if from, err = c.home.Speaker(*as); err == nil {
			target := *to
			if target == "" {
				return harness.ExitUsage, usageErr("ask --as needs --to <alias, run:<id> or room:<name>>")
			}
			id, answer, answered, err = r.AskTo(ctx, from, target, question)
		}
	}
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	if !answered {
		fmt.Fprintf(c.stderr, "harness: no answer to %s within %s; go on with your best judgement and say what you assumed\n", id, *wait)
		return harness.ExitRunning, nil
	}
	fmt.Fprintln(c.stdout, answer)
	fmt.Fprintf(c.stderr, "harness: answer to %s\n", id)
	return 0, nil
}

func (c *cli) reply(args []string) (int, error) {
	fs := c.flags("reply")
	as := fs.String("as", "", "answer a question asked of you (an alias that joined), by its id")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if *as != "" || os.Getenv(harness.EnvAs) != "" && len(pos) >= 2 && strings.HasPrefix(pos[0], "q-") {
		if len(pos) < 2 || !strings.HasPrefix(pos[0], "q-") {
			return harness.ExitUsage, usageErr("reply --as <me> needs <question-id> \"answer\"")
		}
		me, err := c.home.Speaker(*as)
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		to, err := c.home.ReplyTo(me, pos[0], strings.Join(pos[1:], " "))
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		fmt.Fprintf(c.stderr, "harness: answered %s from %s\n", pos[0], to)
		return 0, nil
	}
	if len(pos) < 2 {
		return harness.ExitUsage, usageErr("reply needs <run-id> [question-id] \"answer\", or --as <me> <question-id> \"answer\"")
	}
	id, qid, answer := pos[0], "", strings.Join(pos[1:], " ")
	if len(pos) > 2 && strings.HasPrefix(pos[1], "q-") && !strings.Contains(pos[1], " ") {
		qid, answer = pos[1], strings.Join(pos[2:], " ")
	}
	qid, err = c.home.Reply(id, qid, answer)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: answered %s in run %s\n", qid, id)
	return 0, nil
}

func (c *cli) inbox(args []string) (int, error) {
	fs := c.flags("inbox")
	peek := fs.Bool("peek", false, "show without marking read")
	as := fs.String("as", "", "your alias (a party); inside a run, without it, the run's inbox")
	wait := fs.Duration("wait", 0, "wait this long for something new (exit 7 if nothing came)")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	if *as == "" && os.Getenv(harness.EnvAs) == "" {
		run, msgs, err := c.home.Inbox(*peek)
		if err != nil {
			return harness.ExitUsage, errUsage{fmt.Errorf("%w; a party reads its inbox with --as <alias>", err)}
		}
		for _, m := range msgs {
			fmt.Fprintf(c.stdout, "%s  %s\n", m.Time.Local().Format("15:04:05"), m.Text)
		}
		if len(msgs) == 0 {
			fmt.Fprintf(c.stderr, "harness: no new messages for run %s\n", run.ID)
		}
		return 0, nil
	}
	me, err := c.home.Speaker(*as)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if *wait > 0 {
		ctx, cancel = context.WithTimeout(ctx, *wait)
	} else {
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	items, err := c.home.WaitPartyInbox(ctx, me, *peek)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	if *asJSON {
		type item struct {
			Source string `json:"source"`
			harness.Event
		}
		out := []item{}
		for _, it := range items {
			out = append(out, item{it.Source, it.Event})
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
	} else {
		for _, it := range items {
			fmt.Fprintln(c.stdout, itemLine(it))
		}
	}
	if len(items) == 0 {
		fmt.Fprintf(c.stderr, "harness: nothing new for %s\n", me)
		if *wait > 0 {
			return harness.ExitRunning, nil
		}
	}
	return 0, nil
}

// itemLine is one inbox item: when, where, who, what, and how to answer.
func itemLine(it harness.InboxItem) string {
	e := it.Event
	from := e.From
	if from == "" && strings.HasPrefix(it.Source, "run:") {
		from = it.Source
	}
	line := fmt.Sprintf("%s  %-14s %-8s", e.Time.Local().Format("15:04:05"), it.Source, e.Kind)
	if from != "" {
		line += "  " + from + ":"
	}
	if e.Kind == harness.EvFinished || e.Kind == harness.EvTimeoutSoon {
		line += "  " + e.Name
	}
	line += "  " + e.Text
	switch {
	case e.Kind == harness.EvAsk && strings.HasPrefix(it.Source, "run:") && e.From == "":
		line += fmt.Sprintf("   → harness reply %s %s \"<answer>\"", strings.TrimPrefix(it.Source, "run:"), e.Name)
	case e.Kind == harness.EvAsk:
		line += fmt.Sprintf("   → harness reply --as <me> %s \"<answer>\"", e.Name)
	}
	return line
}

func (c *cli) claim(args []string) (int, error) {
	fs := c.flags("claim")
	as := fs.String("as", "", "who claims (an alias that joined); inside a run, the run")
	ttl := fs.Duration("for", 2*time.Hour, "how long the claim lasts (it also ends when its run ends or its party is away)")
	note := fs.String("note", "", "what you are doing there")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) == 0 {
		return harness.ExitUsage, usageErr("claim needs one or more paths")
	}
	who, err := c.home.Speaker(*as)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	paths, err := absPaths(pos)
	if err != nil {
		return harness.ExitUsage, err
	}
	if err := c.home.ClaimPaths(who, paths, *ttl, *note); err != nil {
		return exitFor(err), err
	}
	fmt.Fprintf(c.stderr, "harness: %s claimed %s for %s\n", who, strings.Join(paths, ", "), *ttl)
	return 0, nil
}

func (c *cli) release(args []string) (int, error) {
	fs := c.flags("release")
	as := fs.String("as", "", "who releases; inside a run, the run")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	who, err := c.home.Speaker(*as)
	if err != nil || who == "" {
		return harness.ExitUsage, usageErr("release needs --as <alias>, or to run inside a run")
	}
	paths, err := absPaths(pos)
	if err != nil {
		return harness.ExitUsage, err
	}
	if err := c.home.Release(who, paths); err != nil {
		return harness.ExitUsage, err
	}
	fmt.Fprintf(c.stderr, "harness: released %s's claims\n", who)
	return 0, nil
}

func (c *cli) claims(args []string) (int, error) {
	fs := c.flags("claims")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	cs, err := c.home.Claims()
	if err != nil {
		return harness.ExitUsage, err
	}
	if *asJSON {
		if cs == nil {
			cs = []harness.PathClaim{}
		}
		data, _ := json.MarshalIndent(cs, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
		return 0, nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tHOLDER\tUNTIL\tNOTE")
	for _, cl := range cs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cl.Path, cl.Holder, cl.Until.Local().Format("15:04"), or(cl.Note, "-"))
	}
	return 0, w.Flush()
}

func absPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.Clean(a))
	}
	return out, nil
}

// watch follows, for a person: with --as, its inbox; otherwise every
// active run's events and every room (or one), until interrupted.
func (c *cli) watch(args []string) (int, error) {
	fs := c.flags("watch")
	as := fs.String("as", "", "follow this party's inbox")
	room := fs.String("room", "", "follow only this room")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *as != "" {
		me, err := c.home.Speaker(*as)
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		fmt.Fprintf(c.stderr, "harness: watching %s's inbox; Ctrl-C to stop\n", me)
		for ctx.Err() == nil {
			items, err := c.home.WaitPartyInbox(ctx, me, false)
			if err != nil {
				return harness.ExitUsage, err
			}
			for _, it := range items {
				fmt.Fprintln(c.stdout, itemLine(it))
			}
		}
		return 0, nil
	}
	fmt.Fprintln(c.stderr, "harness: watching active runs and rooms; Ctrl-C to stop")
	w := harness.NewWatcher(c.home, *room)
	for ctx.Err() == nil {
		for _, it := range w.Poll() {
			fmt.Fprintln(c.stdout, itemLine(it))
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	return 0, nil
}
