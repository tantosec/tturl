package main

import (
	"strings"
	"testing"
)

// TestCommandCatalogue checks the facts every generated command surface needs:
// stable unique identity, complete copy, and a runner. It also checks that both
// lookup paths return the catalogue entry itself.
func TestCommandCatalogue(t *testing.T) {
	ids := make(map[commandID]bool)
	names := make(map[string]bool)
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		if ids[spec.id] {
			t.Errorf("duplicate command ID %d", spec.id)
		}
		ids[spec.id] = true
		if spec.name == "" || names[spec.name] {
			t.Errorf("command name %q is empty or duplicated", spec.name)
		}
		names[spec.name] = true
		if spec.menuSummary == "" || spec.summary == "" ||
			len(spec.example) == 0 || spec.run == nil {
			t.Errorf("command %q has an incomplete catalogue entry", spec.name)
		}
		if !spec.requestGrammar && spec.rootSynopsis == "" {
			t.Errorf("command %q has no root synopsis", spec.name)
		}
		switch spec.lifecycle.status {
		case commandStable:
			if spec.lifecycle.message != "" {
				t.Errorf("stable command %q has a lifecycle message", spec.name)
			}
		case commandBeta, commandDeprecated:
			if spec.lifecycle.message == "" {
				t.Errorf("marked command %q has no lifecycle message", spec.name)
			}
		default:
			t.Errorf("command %q has unknown lifecycle status %d",
				spec.name, spec.lifecycle.status)
		}
		for _, line := range spec.example {
			if strings.TrimSpace(line) != line {
				t.Errorf("command %q example line owns presentation whitespace: %q",
					spec.name, line)
			}
		}
		if commandByID(spec.id) != spec {
			t.Errorf("commandByID(%d) did not return its catalogue entry", spec.id)
		}
		if commandByName(spec.name) != spec {
			t.Errorf("commandByName(%q) did not return its catalogue entry",
				spec.name)
		}
	}
	if commandByName("absent") != nil {
		t.Error("commandByName accepted an unknown command")
	}
}

func TestDetectCataloguePurposeSurfaces(t *testing.T) {
	spec := commandByID(commandDetect)
	if spec.lifecycle.status != commandBeta ||
		spec.lifecycle.message == "" {
		t.Fatalf("detect lifecycle = %+v", spec.lifecycle)
	}
	wantCompletion := spec.lifecycle.labelledSummary(spec.menuSummary)
	root := completeRoot("det")
	if len(root) != 1 || root[0].value != "detect" ||
		root[0].description != wantCompletion {
		t.Errorf("root completion = %+v, want detect description %q",
			root, wantCompletion)
	}
	help := completeCommandLine([]string{toolName, "help", "det"}, 2)
	wantCompletion = spec.lifecycle.labelledSummary(spec.summary)
	if len(help) != 1 || help[0].value != "detect" ||
		help[0].description != wantCompletion {
		t.Errorf("help completion = %+v", help)
	}
}

func TestEveryRequestCommandHasCompletionGrammar(t *testing.T) {
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		if !spec.requestGrammar {
			continue
		}
		parser := describeRequestCommand(spec)
		if options := parser.CompletionOptions(); len(options) == 0 {
			t.Errorf("command %q has no completion options", spec.name)
		}
	}
}

func TestCommandLifecycleSurfaces(t *testing.T) {
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		if !spec.requestGrammar {
			continue
		}
		wantNotice := spec.lifecycle.notice(spec.name)
		menuSummary := spec.lifecycle.labelledSummary(spec.menuSummary)
		topicSummary := spec.lifecycle.labelledSummary(spec.summary)
		if !strings.Contains(
			strings.Join(strings.Fields(topLevelUsage), " "), menuSummary,
		) {
			t.Errorf("root help omits %s lifecycle summary %q:\n%s",
				spec.name, menuSummary, topLevelUsage)
		}

		layout := newCommand(spec)
		help := renderCommandUsage(spec, layout.parser)
		heading, body, ok := strings.Cut(help, "\n\n")
		wantHeading := toolName + " " + spec.name + " - " + spec.summary
		if !ok || strings.Join(strings.Fields(heading), " ") != wantHeading {
			t.Errorf("%s usage heading does not derive from its catalogue:\n%s", spec.name, help)
		}
		usageTop := ""
		if wantNotice != "" {
			usageTop += "Notice: " + wantNotice + "\n\n"
		}
		usageTop += "Usage:\n"
		if !strings.HasPrefix(body, usageTop) {
			t.Errorf("%s help does not reflect its lifecycle:\n%s", spec.name, help)
		}

		topic, ok := lookupHelpTopic(spec.name)
		if !ok || topic.summary != topicSummary || topic.command != spec {
			t.Errorf("%s topic does not derive from its command: %+v", spec.name, topic)
			continue
		}
		rendered := renderHelpTopic(topic)
		before, after, ok := strings.Cut(rendered, "\n\n")
		if !ok {
			t.Errorf("%s topic has no heading boundary:\n%s", spec.name, rendered)
			continue
		}
		wantHeading = "# " + toolName + " help " + spec.name + " - " +
			spec.summary
		if got := strings.Join(strings.Fields(before), " "); got != wantHeading {
			t.Errorf("%s topic heading = %q, want %q", spec.name, got, wantHeading)
		}
		wantTopicBody := after
		if wantNotice != "" {
			if !strings.HasPrefix(
				wantTopicBody, "Notice: "+wantNotice+"\n\n",
			) {
				t.Errorf("%s topic does not reflect its lifecycle:\n%s",
					spec.name, rendered)
			}
		} else if strings.HasPrefix(wantTopicBody, "Notice: ") {
			t.Errorf("stable %s topic has a lifecycle notice:\n%s",
				spec.name, rendered)
		}
	}
}

func TestCommandLifecycleNotice(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle commandLifecycle
		want      string
	}{
		{name: "stable"},
		{
			name: "beta",
			lifecycle: commandLifecycle{
				status: commandBeta, message: "Expect change.",
			},
			want: "probe is beta. Expect change.",
		},
		{
			name: "deprecated",
			lifecycle: commandLifecycle{
				status: commandDeprecated, message: "Use 'replacement'.",
			},
			want: "probe is deprecated. Use 'replacement'.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.lifecycle.notice("probe"); got != test.want {
				t.Errorf("notice = %q, want %q", got, test.want)
			}
		})
	}
}

// TestCatalogueGeneratesCommandSurfaces checks that catalogue order drives
// root help and sibling references, while each entry frames its own page.
func TestCatalogueGeneratesCommandSurfaces(t *testing.T) {
	flatRoot := strings.Join(strings.Fields(topLevelUsage), " ")
	at := 0
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		pos := strings.Index(flatRoot[at:], spec.menuSummary)
		if pos < 0 {
			t.Fatalf("root help omits or misorders %q:\n%s",
				spec.menuSummary, topLevelUsage)
		}
		at += pos + len(spec.menuSummary)
		if !spec.requestGrammar {
			continue
		}

		layout := newCommand(spec)
		if layout.parser.Name != toolName+" "+spec.name ||
			layout.parser.Description != spec.summary {
			t.Errorf("command %q framing does not match its catalogue entry",
				spec.name)
		}
		heading, _, ok := strings.Cut(layout.parser.Usage(), "\n\n")
		if !ok || strings.Join(strings.Fields(heading), " ") != layout.parser.Name+" - "+spec.summary {
			t.Errorf("command %q heading does not match its catalogue entry:\n%s",
				spec.name, layout.parser.Usage())
		}
		if len(layout.parser.Synopses) != 2 {
			t.Errorf("command %q has %d synopses, want 2",
				spec.name, len(layout.parser.Synopses))
		}
		usage := layout.parser.Usage()
		wantGuide := "Usage:\n  " + toolName + " " + spec.name +
			" [preamble flags] URL [--block ...]\n" +
			"  " + toolName + " " + spec.name +
			" [preamble flags] --block [URL...] [block flags] ...\n\n" +
			"Guide:\n  " + toolName + " help " + spec.name + "\n\n"
		if !strings.Contains(usage, wantGuide) {
			t.Errorf("%s guide does not immediately follow Usage:\n%s",
				spec.name, usage)
		}
		deliveryGuide := toolName + " help delivery"
		if spec.id == commandTime {
			deliveryGuide = toolName + " help time"
		}
		for _, route := range []string{
			toolName + " help requests",
			deliveryGuide,
			toolName + " help output",
		} {
			if !strings.Contains(usage, route) {
				t.Errorf("%s help omits detailed route %q:\n%s",
					spec.name, route, usage)
			}
		}
	}

	demo := commandByID(commandDemoServer)
	usage := renderDemoServerUsage(demo)
	if !strings.HasPrefix(usage, toolName+" demo-server - "+demo.summary) ||
		!strings.Contains(usage, "Guide:\n  "+toolName+" help demo-server") {
		t.Errorf("demo-server help does not derive catalogue framing:\n%s", usage)
	}
}

// TestCommandLayoutFixesSectionOrder checks that registrar call order cannot
// move tturl's standard help sections. Each handle is deliberately
// populated in reverse display order.
func TestCommandLayoutFixesSectionOrder(t *testing.T) {
	layout := newCommand(commandByID(commandRace))
	layout.execution.Bool("execute", "", false, "execution flag")
	layout.output.Bool("show", "", false, "output flag")
	layout.command.Bool("race-control", "", false, "command flag")

	usage := layout.parser.Usage()
	want := []string{
		"Race options:", "--race-control",
		sectionExec + ":", "--execute",
		"Request options:",
		"Block options:",
		"Output options:", "--show",
		"Common options:",
	}
	at := 0
	for _, text := range want {
		i := strings.Index(usage[at:], text)
		if i < 0 {
			t.Fatalf("help omits %q or has it out of order:\n%s", text, usage)
		}
		at += i + len(text)
	}
}
