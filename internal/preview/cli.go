package preview

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ross-cripto/remote-dev/internal/panel"
	"github.com/Ross-cripto/remote-dev/internal/project"
)

const version = "0.3.0-rc.1"

const usage = `Remote Dev
Edit locally. Run on your Linux box.

Usage:
  remote-dev connect [--name NAME] [--identity PATH] [--prepare] [SSH_TARGET]
  remote-dev doctor [--host HOST]
  remote-dev up [--host HOST] [--no-watch]
  remote-dev test [--host HOST] [--service SERVICE]
  remote-dev sync | status | logs | open | stop
  remote-dev migrate [--host HOST]
  remote-dev init --host HOST [--mode compose|commands|node]
  remote-dev list | tui
  remote-dev --version

Start from a Git worktree with Compose, a supported Node project, or remote-dev.toml.
`

func commandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func connect(ctx context.Context, writer io.Writer, client Client, arguments []string) error {
	flags := commandFlags("connect")
	identity := flags.String("identity", "", "local OpenSSH identity path")
	name := flags.String("name", "", "saved connection name")
	prepare := flags.Bool("prepare", false, "offer to install missing Docker prerequisites")
	target := ""
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		target, arguments = arguments[0], arguments[1:]
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() == 1 && target == "" {
		target = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return errors.New("usage: remote-dev connect [options] [SSH_TARGET]")
	}
	if *name != "" && !sshName.MatchString(*name) {
		return errors.New("connection names may use letters, numbers, dots, underscores and hyphens")
	}

	book, err := client.connectionBook()
	if err != nil {
		return err
	}
	label := *name
	if label == "" {
		label = target
	}
	if label == "" {
		label = book.Default
	}
	if target == "" {
		saved, exists := book.Hosts[label]
		if !exists {
			return errors.New("give an SSH alias or user@server to register a server")
		}
		target = saved.Target
	}
	saved := book.Hosts[label]
	if *identity == "" && saved.Target != "" {
		*identity = saved.Identity
	}
	connection, err := newConnection(target, *identity)
	if err != nil {
		return err
	}
	if saved.Target != "" && saved.Target != connection.Target {
		return fmt.Errorf("connection %q points to another server. Choose another --name", label)
	}
	output, isFile := writer.(*os.File)
	facts, err := connection.probe(ctx, isFile && panel.IsTerminal(os.Stdin, output))
	if err != nil {
		return err
	}
	if err := facts.ready(); err != nil {
		if !*prepare {
			return err
		}
		facts, err = connection.prepare(ctx, writer, facts)
		if err != nil {
			return err
		}
	}
	connection, detail, err := connection.prepared(ctx, facts)
	if err != nil {
		return err
	}
	if err := client.saveConnection(label, connection); err != nil {
		return err
	}
	fmt.Fprintf(writer, "Connected to %s as %s. %s\n", connection.Target, label, detail)
	return nil
}

func (client Client) panel(ctx context.Context, output *os.File) error {
	root, _ := worktreeRoot(ctx)
	discover := func(context.Context) ([]panel.Workspace, error) {
		workspaces, err := client.workspaces()
		if err != nil {
			return nil, err
		}
		entries := make([]panel.Workspace, 0, len(workspaces)+1)
		if root != "" {
			mode, host := "Unconfigured", ""
			if detected, err := project.Load(root); err == nil {
				mode, host = detected.Mode, detected.Host
				if connection, err := client.resolveConnection(host); err == nil {
					host = connection.Target
				}
			}
			for _, saved := range workspaces {
				if saved.Root == root {
					host = saved.Connection.Target
					break
				}
			}
			entries = append(entries, panel.Workspace{Root: root, Name: filepath.Base(root), Host: host, Mode: mode, Current: true})
		}
		for _, saved := range workspaces {
			if saved.Root == root {
				continue
			}
			if info, err := os.Stat(saved.Root); err == nil && info.IsDir() {
				entries = append(entries, panel.Workspace{Root: saved.Root, Name: filepath.Base(saved.Root), Host: saved.Connection.Target, Mode: saved.Mode})
			}
		}
		return entries, nil
	}
	invoke := func(operationContext context.Context, action string, workspace panel.Workspace) (string, error) {
		var result bytes.Buffer
		if action == "start" {
			err := client.up(operationContext, &result, workspace.Root, "", true)
			return result.String(), err
		}
		err := client.workspaceAction(operationContext, &result, workspace.Root, action)
		return result.String(), err
	}
	return panel.Run(ctx, os.Stdin, output, discover, invoke)
}

func Run(ctx context.Context, writer io.Writer, arguments []string) error {
	if len(arguments) == 0 {
		output, isFile := writer.(*os.File)
		if isFile && panel.IsTerminal(os.Stdin, output) {
			return Run(ctx, writer, []string{"tui"})
		}
		fmt.Fprint(writer, usage)
		return nil
	}
	action, operands := arguments[0], arguments[1:]
	switch action {
	case "help", "--help", "-h":
		fmt.Fprint(writer, usage)
		return nil
	case "--version":
		fmt.Fprintln(writer, version)
		return nil
	case "connect", "doctor", "up", "test", "sync", "status", "logs", "open", "stop", "list", "tui", "migrate", "init", "__watch":
	default:
		return fmt.Errorf("unknown command %q. Run remote-dev --help", action)
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	switch action {
	case "connect":
		return connect(ctx, writer, client, operands)
	case "__watch":
		if len(operands) != 2 {
			return errors.New("watcher requires a worktree and lease")
		}
		return client.watch(ctx, operands[0], operands[1])
	case "list":
		if len(operands) != 0 {
			return errors.New("usage: remote-dev list")
		}
		return client.list(writer)
	case "tui":
		output, isFile := writer.(*os.File)
		if len(operands) != 0 || !isFile || !panel.IsTerminal(os.Stdin, output) {
			return errors.New("remote-dev tui requires an interactive terminal")
		}
		return client.panel(ctx, output)
	case "doctor":
		flags := commandFlags(action)
		host := flags.String("host", "", "SSH alias or saved connection")
		if err := flags.Parse(operands); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("usage: remote-dev doctor [--host HOST]")
		}
		connection, err := client.resolveConnection(*host)
		if err != nil {
			return err
		}
		detail, err := connection.doctor(ctx, false)
		if err != nil {
			return err
		}
		fmt.Fprintln(writer, detail)
		return nil
	}

	root, err := worktreeRoot(ctx)
	if err != nil {
		return err
	}
	flags := commandFlags(action)
	host := flags.String("host", "", "SSH alias or saved connection")
	var noWatch *bool
	var mode *string
	var testService *string
	switch action {
	case "up":
		noWatch = flags.Bool("no-watch", false, "sync manually")
	case "test":
		testService = flags.String("service", "", "Compose or configured test service")
	case "init":
		mode = flags.String("mode", "compose", "compose, commands or node")
	}
	if err := flags.Parse(operands); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q for remote-dev %s", flags.Arg(0), action)
	}
	switch action {
	case "up":
		return client.up(ctx, writer, root, *host, !*noWatch)
	case "test":
		return client.runTests(ctx, writer, root, *host, *testService)
	case "migrate":
		return client.migrate(ctx, writer, root, *host)
	case "init":
		if *host == "" {
			return errors.New("usage: remote-dev init --host HOST [--mode compose|commands|node]")
		}
		path, err := project.Init(root, *host, *mode)
		if err == nil {
			fmt.Fprintf(writer, "Created %s. Edit it, then run remote-dev up.\n", path)
		}
		return err
	}
	if *host != "" {
		workspace, err := client.readWorkspace(root)
		if err != nil {
			return err
		}
		connection, err := client.resolveConnection(*host)
		if err != nil {
			return err
		}
		if workspace.Connection.Target != connection.Target {
			return errors.New("the selected host differs from this worktree's registered server")
		}
	}
	return client.workspaceAction(ctx, writer, root, action)
}
