package main

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

const helpCommandName = "help"

const helpHelpText = "There's only so much I can do\n"

// helpTopic is one entry in tturl's help namespace.
type helpTopic struct {
	name    string
	summary string
	text    string
	command *commandSpec
}

type helpSection struct {
	title  string
	topics []helpTopic
}

//go:embed helptext
var helpTextFS embed.FS

var helpSections = []helpSection{
	{
		title: "Learn",
		topics: []helpTopic{
			{
				name:    "getting-started",
				summary: "take a guided tour with the local demo server",
				text:    mustReadHelpText("getting-started.txt"),
			},
			{
				name:    "concepts",
				summary: "understand batch release, position, and arrival rank",
				text:    mustReadHelpText("concepts.txt"),
			},
			{
				name:    "commands",
				summary: "choose arrival-order evidence or request durations",
				text:    mustReadHelpText("commands.txt"),
			},
		},
	},
	{
		title: "Command guides",
		topics: []helpTopic{
			commandHelpTopic(commandRace),
			commandHelpTopic(commandMeasure),
			commandHelpTopic(commandAnalyse),
			commandHelpTopic(commandDetect),
			commandHelpTopic(commandTime),
		},
	},
	{
		title: "Build requests",
		topics: []helpTopic{
			{
				name:    "requests",
				summary: "build and inspect the concrete request set",
				text:    mustReadHelpText("requests.txt"),
			},
			{
				name:    "blocks",
				summary: "use the preamble, blocks, and inheritance",
				text:    mustReadHelpText("blocks.txt"),
			},
			{
				name:    "headers",
				summary: "set headers, authentication, cookies, and methods",
				text:    mustReadHelpText("headers.txt"),
			},
			{
				name:    "bodies",
				summary: "build data, multipart, and JSON request bodies",
				text:    mustReadHelpText("bodies.txt"),
			},
			{
				name:    "vary",
				summary: "expand request values and label the variants",
				text:    mustReadHelpText("vary.txt"),
			},
		},
	},
	{
		title: "Run experiments",
		topics: []helpTopic{
			{
				name:    "design",
				summary: "turn a timing question into credible evidence",
				text:    mustReadHelpText("design.txt"),
			},
			{
				name:    "delivery",
				summary: "control load, connections, and body release",
				text:    mustReadHelpText("delivery.txt"),
			},
			{
				name:    "padding",
				summary: "apply heuristic padding by outbound position",
				text:    mustReadHelpText("padding.txt"),
			},
		},
	},
	{
		title: "Use output",
		topics: []helpTopic{
			{
				name:    "output",
				summary: "route reports and read text evidence",
				text:    mustReadHelpText("output.txt"),
			},
			{
				name:    "json",
				summary: "consume versioned JSON and JSONL reports",
				text:    mustReadHelpText("json.txt"),
			},
			commandHelpTopic(commandSchema),
		},
	},
	{
		title: "Utilities",
		topics: []helpTopic{
			{
				name:    "demo-server",
				summary: "use the safe local learning and research target",
				text:    mustReadHelpText("demo-server.txt"),
			},
			{
				name:    "completion",
				summary: "enable tab completion for commands and options",
				text:    mustReadHelpText("completion.txt"),
			},
		},
	},
}

var helpTopics = flattenHelpSections(helpSections)

func commandHelpTopic(id commandID) helpTopic {
	spec := commandByID(id)
	return helpTopic{
		name:    spec.name,
		summary: spec.lifecycle.labelledSummary(spec.summary),
		command: spec,
		text:    mustReadHelpText(spec.name + ".txt"),
	}
}

func mustReadHelpText(name string) string {
	b, err := helpTextFS.ReadFile("helptext/" + name)
	if err != nil {
		panic(toolName + ": reading embedded help text: " + err.Error())
	}
	text := normaliseHelpText(string(b))
	if text == "" {
		panic(toolName + ": embedded help text is empty: " + name)
	}
	return text
}

// normaliseHelpText makes the document boundary independent of how its source
// was terminated. Internal whitespace remains author-controlled.
func normaliseHelpText(text string) string {
	text = strings.TrimRight(text, " \t\r\n")
	if text == "" {
		return ""
	}
	return text + "\n"
}

func flattenHelpSections(sections []helpSection) []helpTopic {
	var topics []helpTopic
	for _, section := range sections {
		topics = append(topics, section.topics...)
	}
	return topics
}

var helpIndex = renderHelpIndex(helpSections)

func renderHelpIndex(sections []helpSection) string {
	var b strings.Builder
	b.WriteString(toolName + " help - detailed help by topic\n\n")
	b.WriteString("Usage:\n")
	b.WriteString("  " + toolName + " help [OPTIONS] [TOPIC]\n\n")
	b.WriteString("Options (before TOPIC):\n  -s, --silent       " + silentDescription + "\n\n")

	nameWidth := 0
	for _, section := range sections {
		for _, topic := range section.topics {
			nameWidth = max(nameWidth, len(topic.name))
		}
	}
	for _, section := range sections {
		b.WriteString(section.title)
		b.WriteString(":\n")
		const spacing = "  "
		descriptionCol := len("  ") + nameWidth + len(spacing)
		for _, topic := range section.topics {
			lines := wrapRootMenuSummary(
				topic.summary, textWidth-descriptionCol)
			fmt.Fprintf(&b, "  %-*s%s%s\n",
				nameWidth, topic.name, spacing, lines[0])
			for _, line := range lines[1:] {
				fmt.Fprintf(&b, "%*s%s\n", descriptionCol, "", line)
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString("Run '" + toolName + " -h' for the overview.\n")
	b.WriteString("Run '" + toolName +
		" <command> -h' for a complete flag reference.\n")
	return normaliseHelpText(b.String())
}

func lookupHelpTopic(name string) (helpTopic, bool) {
	for _, topic := range helpTopics {
		if topic.name == name {
			return topic, true
		}
	}
	return helpTopic{}, false
}

func renderHelpTopic(topic helpTopic) string {
	var b strings.Builder
	headingSummary := topic.summary
	if topic.command != nil {
		headingSummary = topic.command.summary
	}
	writeWrappedASCII(
		&b, "# "+toolName+" help "+topic.name+" - ", headingSummary)
	b.WriteByte('\n')
	if topic.command != nil {
		if notice := topic.command.lifecycle.notice(topic.command.name); notice != "" {
			b.WriteString("Notice: ")
			b.WriteString(notice)
			b.WriteString("\n\n")
		}
	}
	b.WriteString(topic.text)
	return normaliseHelpText(b.String())
}

func runHelpCommand(args []string, out commandOutput) (code int, err error) {
	args, out = silentPrefix(args, out)
	if len(args) == 0 ||
		(len(args) == 1 && (args[0] == "-h" || args[0] == "--help")) {
		if _, err := fmt.Fprint(out.stdout, helpIndex); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if len(args) > 1 {
		commandErr := errors.New("help accepts at most one topic")
		writeErr := writeDiagnosticDocument(
			out.stderr, diagnosticError, commandErr.Error(), helpIndex)
		return 2, errors.Join(commandErr, writeErr)
	}
	if args[0] == helpCommandName {
		if _, err := fmt.Fprint(out.stdout, helpHelpText); err != nil {
			return 1, err
		}
		return 0, nil
	}

	topic, ok := lookupHelpTopic(args[0])
	if !ok {
		commandErr := fmt.Errorf(
			"unknown help topic %q", curlblocks.DisplayText(args[0]))
		writeErr := writeDiagnosticDocument(
			out.stderr, diagnosticError, commandErr.Error(), helpIndex)
		return 2, errors.Join(commandErr, writeErr)
	}
	if _, err := fmt.Fprint(out.stdout, renderHelpTopic(topic)); err != nil {
		return 1, err
	}
	return 0, nil
}
