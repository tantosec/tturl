package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
)

type commandID uint8

const (
	commandRace commandID = iota
	commandMeasure
	commandAnalyse
	commandDetect
	commandTime
	commandDemoServer
	commandSchema
)

type commandRunner func(
	context.Context,
	*commandSpec,
	[]commandSpec,
	[]string,
	commandOutput,
) error

type commandStatus uint8

const (
	commandStable commandStatus = iota
	commandBeta
	commandDeprecated
)

type commandLifecycle struct {
	status  commandStatus
	message string
}

func (l commandLifecycle) label() string {
	switch l.status {
	case commandStable:
		return ""
	case commandBeta:
		return "beta"
	case commandDeprecated:
		return "deprecated"
	default:
		panic(fmt.Sprintf("%s: unknown command status %d", toolName, l.status))
	}
}

func (l commandLifecycle) labelledSummary(summary string) string {
	if label := l.label(); label != "" {
		return "[" + label + "] " + summary
	}
	return summary
}

func (l commandLifecycle) notice(name string) string {
	label := l.label()
	if label == "" {
		return ""
	}
	return name + " is " + label + ". " + l.message
}

// commandSpec is one command's complete catalogue entry. The ordered catalogue
// drives dispatch, root help, command framing, and references to sibling
// commands.
type commandSpec struct {
	id             commandID
	name           string
	menuSummary    string
	summary        string
	example        []string
	rootSynopsis   string
	requestGrammar bool
	lifecycle      commandLifecycle
	schema         *schemaDescriptor
	run            commandRunner
}

var commandCatalogue = []commandSpec{
	{
		id:             commandRace,
		name:           "race",
		menuSummary:    "run trials; inspect every response and arrival order",
		summary:        "run trials and inspect every response and arrival order",
		requestGrammar: true,
		example: []string{
			toolName + " race -k \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=100us' --name fast \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=5000us' --name slow",
		},
		schema: &raceSchema,
		run:    runRaceCommand,
	},
	{
		id:             commandMeasure,
		name:           "measure",
		menuSummary:    "describe arrival ranks across a repeated fixed batch",
		summary:        "describe arrival ranks across a repeated fixed batch",
		requestGrammar: true,
		example: []string{
			toolName + " measure -k --trials 20 \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=100us' --name fast \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=5000us' --name slow",
		},
		schema: &measureSchema,
		run:    runMeasureCommand,
	},
	{
		id:             commandAnalyse,
		name:           "analyse",
		menuSummary:    "test a balanced batch for an arrival-order difference",
		summary:        "test a balanced batch for an arrival-order difference",
		requestGrammar: true,
		example: []string{
			toolName + " analyse -k \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=100us' --name fast \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=5000us' --name slow",
		},
		schema: &analyseSchema,
		run:    runAnalyseCommand,
	},
	{
		id:             commandDetect,
		name:           "detect",
		menuSummary:    "adaptively test for an early or late arrival-order outlier",
		summary:        "adaptively test for an early or late arrival outlier",
		requestGrammar: true,
		lifecycle: commandLifecycle{
			status:  commandBeta,
			message: "Its interface and behaviour may change.",
		},
		example: []string{
			toolName + " detect -k --direction late --comparisons-max 200 \\",
			"--block '127.0.0.1:8443/sleep?duration=D' --vary 'D={100us,5000us}'",
		},
		schema: &detectSchema,
		run:    runDetectCommand,
	},
	{
		id:             commandTime,
		name:           "time",
		menuSummary:    "describe per-request durations on separate connections",
		summary:        "measure per-request durations on separate connections",
		requestGrammar: true,
		lifecycle: commandLifecycle{
			status:  commandBeta,
			message: "Its interface and behaviour may change.",
		},
		example: []string{
			toolName + " time -k --trials 20 \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=100us' --name short \\",
			"--block 'https://127.0.0.1:8443/sleep?duration=5000us' --name long",
		},
		schema: &timeSchema,
		run:    runTimeCommand,
	},
	{
		id:             commandDemoServer,
		name:           "demo-server",
		menuSummary:    "start a local target for timing and request-race exercises",
		summary:        "run a local HTTP/2 education and research target",
		example:        []string{toolName + " demo-server"},
		rootSynopsis:   "[options]",
		requestGrammar: false,
		run:            runDemoServerCommand,
	},
	{
		id:           commandSchema,
		name:         "schema",
		menuSummary:  "export shipped JSON Schema documents offline",
		summary:      "export shipped JSON Schema documents offline",
		example:      []string{toolName + " schema race --output race.schema.json"},
		rootSynopsis: "[OPTIONS] SUBJECT",
		run:          runSchemaCommand,
	},
}

func commandByID(id commandID) *commandSpec {
	for i := range commandCatalogue {
		if commandCatalogue[i].id == id {
			return &commandCatalogue[i]
		}
	}
	panic(fmt.Sprintf("%s: command ID %d is not in the catalogue", toolName, id))
}

func commandByName(name string) *commandSpec {
	for i := range commandCatalogue {
		if commandCatalogue[i].name == name {
			return &commandCatalogue[i]
		}
	}
	return nil
}

const (
	sectionOutput = "Output options"
	sectionExec   = "Response and live limits"
)

// commandLayout is tturl's standard presentation atop a curlblocks Parser.
// Creating every section here fixes their display positions before registrars
// populate them.
type commandLayout struct {
	parser    *curlblocks.Parser
	command   *curlblocks.Registry
	output    *curlblocks.Registry
	execution *curlblocks.Registry
	release   *curlblocks.Registry
	preview   *curlblocks.Registry
	buildInfo buildinfo.Info
}

func newCommand(spec *commandSpec) commandLayout {
	if !spec.requestGrammar {
		panic(toolName + ": request command layout used for " + spec.name)
	}
	info := buildinfo.Current()
	p := curlblocks.New(
		curlblocks.WithUserAgent(toolName+"/"+info.Version()),
		// Fill a missing scheme before command-specific protocol validation.
		curlblocks.WithDefaultScheme("https"),
		curlblocks.WithMaxFanSize(maxFanSize),
		curlblocks.WithInsecureFlag(),
		curlblocks.WithVersionFlag(info.Diagnostic(toolName)),
	)
	p.Name = toolName + " " + spec.name
	p.Description = spec.summary
	p.Synopses = []string{
		"[preamble flags] URL [--block ...]",
		"[preamble flags] --block [URL...] [block flags] ...",
	}
	p.Guide = toolName + " help " + spec.name
	p.ScopeNote = "Command-wide flags precede the first --block. Request flags " +
		"before it are defaults for every block; request flags inside a block " +
		"affect only that block. Blocks inherit from the preamble. " +
		"Block URLs replace preamble URLs; inherited headers/data use their " +
		"own add rules. Block-only flags require --block."
	p.UsageWidth = textWidth
	p.Examples = []curlblocks.UsageExample{{Lines: spec.example}}
	p.DetailedHelp = []curlblocks.HelpRoute{
		{
			Invocation: toolName + " help requests",
			Summary:    "request blocks, headers, bodies, and expansion",
		},
		{
			Invocation: toolName + " help delivery",
			Summary:    "connections, pacing, stream limits, and body release",
		},
	}
	if spec.id == commandTime {
		p.DetailedHelp[1] = curlblocks.HelpRoute{
			Invocation: toolName + " help time",
			Summary:    "duration populations, connections, pacing, and release",
		}
	}
	p.DetailedHelp = append(p.DetailedHelp, curlblocks.HelpRoute{
		Invocation: toolName + " help output",
		Summary:    "reports, completion, and capture",
	})
	commandTitle := strings.ToUpper(spec.name[:1]) + spec.name[1:] + " options"
	if spec.id == commandDetect {
		commandTitle = "Detect decisions (before --block)"
	}
	if spec.id == commandTime {
		commandTitle = "Experiment"
	}
	command := p.Global.Section(commandTitle)
	p.Global.Section("Connections")
	p.Global.Section("Pacing")
	var execution, release *curlblocks.Registry
	if spec.id == commandTime {
		release = p.Global.Section("Request delivery")
		execution = p.Global.Section("Timing")
	} else {
		execution = p.Global.Section(sectionExec)
		release = p.Global.Section("Body release and stream policy")
	}
	preview := p.Global.Section("Delivery preview")
	return commandLayout{
		parser:    p,
		command:   command,
		execution: execution,
		release:   release,
		preview:   preview,
		output:    p.Global.TrailingSection(sectionOutput),
		buildInfo: info,
	}
}

// describeRequestCommand builds the same parser and registrations used to run
// one request command, without parsing arguments or performing any I/O. Shell
// completion uses the resulting authoritative grammar.
func describeRequestCommand(spec *commandSpec) *curlblocks.Parser {
	layout := newCommand(spec)
	switch spec.id {
	case commandRace:
		configureRaceCommand(layout)
	case commandMeasure:
		configureMeasureCommand(layout)
	case commandAnalyse:
		configureAnalyseCommand(layout)
	case commandDetect:
		configureDetectCommand(layout)
	case commandTime:
		configureTimeCommand(layout)
	default:
		panic(toolName + ": request command has no grammar description: " +
			spec.name)
	}
	return layout.parser
}

func renderCommandUsage(spec *commandSpec, p *curlblocks.Parser) string {
	usage := p.Usage()
	notice := spec.lifecycle.notice(spec.name)
	if notice == "" {
		return usage
	}
	headingEnd := strings.Index(usage, "\n\n")
	if headingEnd < 0 {
		panic(toolName + ": command usage has no heading boundary")
	}
	headingEnd += 2
	return usage[:headingEnd] + "Notice: " + notice + "\n\n" + usage[headingEnd:]
}

var topLevelUsage = renderTopLevelUsage(commandCatalogue)

type rootMenuItem struct {
	name    string
	summary string
}

func writeRootMenu(
	b *strings.Builder,
	heading string,
	items []rootMenuItem,
) {
	b.WriteString(heading)
	b.WriteString(":\n")
	nameWidth := 0
	for _, item := range items {
		nameWidth = max(nameWidth, len(item.name))
	}
	const nameSpacing = 3
	descriptionCol := 2 + nameWidth + nameSpacing
	for _, item := range items {
		lines := wrapRootMenuSummary(item.summary, textWidth-descriptionCol)
		fmt.Fprintf(b, "  %-*s%s%s\n", nameWidth, item.name,
			strings.Repeat(" ", nameSpacing), lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(b, "%*s%s\n", descriptionCol, "", line)
		}
	}
	b.WriteByte('\n')
}

func wrapRootMenuSummary(summary string, width int) []string {
	words := strings.Fields(summary)
	if len(words) == 0 {
		return []string{""}
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(word) <= width {
			lines[last] += " " + word
			continue
		}
		lines = append(lines, word)
	}
	return lines
}

func renderTopLevelUsage(catalogue []commandSpec) string {
	var b strings.Builder
	b.WriteString(toolName + " - compare response arrival order or measure request durations\n\n")
	b.WriteString("Timeless timing attacks release HTTP/2 request batches and " +
		"compare\n")
	b.WriteString("relative response arrival order instead of elapsed response " +
		"time. The\n")
	b.WriteString("requests form a batch; one race of that complete batch is a " +
		"trial.\n\n")
	b.WriteString("tturl time provides traditional time-based measurements over HTTP/1.1 or\n")
	b.WriteString("HTTP/2. Requests run sequentially by default; optional synchronisation aligns\n")
	b.WriteString("release across separate connections.\n\n")
	b.WriteString("Usage:\n")
	b.WriteString("  " + toolName +
		" <command> [preamble flags] URL [--block ...]\n")
	b.WriteString("  " + toolName +
		" <command> [preamble flags] --block [URL...] " +
		"[block flags] ...\n")
	b.WriteString("  " + toolName + " help [OPTIONS] [TOPIC]\n")
	b.WriteString("  " + toolName + " completion [OPTIONS] SHELL\n")
	for _, spec := range catalogue {
		if !spec.requestGrammar {
			b.WriteString("  ")
			b.WriteString(toolName)
			b.WriteByte(' ')
			b.WriteString(spec.name)
			b.WriteByte(' ')
			b.WriteString(spec.rootSynopsis)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')

	var commands []rootMenuItem
	var utilities []rootMenuItem
	for _, spec := range catalogue {
		summary := spec.lifecycle.labelledSummary(spec.menuSummary)
		if spec.requestGrammar {
			commands = append(commands, rootMenuItem{
				name: spec.name, summary: summary,
			})
		} else {
			utilities = append(utilities, rootMenuItem{name: spec.name, summary: summary})
		}
	}
	writeRootMenu(&b, "Commands", commands)

	utilities = append(utilities, rootMenuItem{
		name:    completionCommandName,
		summary: "generate completion for Bash, Zsh, or fish",
	})
	writeRootMenu(&b, "Utilities", utilities)

	writeRootMenu(&b, "Help", []rootMenuItem{
		{
			name:    helpCommandName,
			summary: "take the guided tour or browse detailed topics",
		},
		{
			name:    "<command> -h",
			summary: "show that command's complete flag reference",
		},
	})

	b.WriteString("Global options:\n")
	b.WriteString("  -V, --version   show the version and exit\n")
	b.WriteString("  -h, --help      show this help and exit\n\n")
	b.WriteString("Copyright (c) 2026 Tanto Security\n")
	return normaliseHelpText(b.String())
}
