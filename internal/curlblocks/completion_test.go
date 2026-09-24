package curlblocks

import (
	"reflect"
	"testing"
)

func TestCompletionOptionsDescribeAcceptedGrammar(t *testing.T) {
	p := New(WithInsecureFlag(), WithVersionFlag("example 1.0"))
	p.Global.String("mode", "m", "MODE", "one", "select a mode")
	p.Shared.Bool("trace", "", false, "trace this request")
	p.Block.Int("copies", "c", "N", 1, "copy this block")

	options := p.CompletionOptions()
	for _, want := range []CompletionOption{
		{
			Names:       []string{"-k", "--insecure"},
			Description: "skip TLS certificate verification",
			Scope:       CompletionGlobal,
		},
		{
			Names:       []string{"-V", "--version"},
			Description: "show the version and exit",
			Scope:       CompletionGlobal,
		},
		{
			Names: []string{"-m", "--mode"}, Argument: "MODE",
			Description: "select a mode", Scope: CompletionGlobal,
			TakesValue: true,
		},
		{
			Names: []string{"-H", "--header"}, Argument: "HEADER",
			Description: headerUsageMain, Scope: CompletionShared,
			TakesValue: true,
		},
		{
			Names: []string{"--data-binary"}, Argument: "DATA",
			Description: dataUsageBinary, Scope: CompletionShared,
			TakesValue: true, FileStyle: CompletionAtFiles,
		},
		{
			Names: []string{"--vary-mode"}, Argument: "MODE",
			Description: "clusterbomb: all combinations; pitchfork: pairs, stopping at shortest binding",
			Values:      []string{"clusterbomb", "pitchfork"},
			Scope:       CompletionShared, TakesValue: true,
		},
		{
			Names:       []string{"--trace"},
			Description: "trace this request", Scope: CompletionShared,
		},
		{
			Names: []string{"--block"}, Argument: "[URL...]",
			Description: "open a block with optional request URLs",
			Scope:       CompletionShared, OpensBlock: true,
		},
		{
			Names: []string{"-c", "--copies"}, Argument: "N",
			Description: "copy this block", Scope: CompletionBlock,
			TakesValue: true,
		},
		{
			Names:       []string{"-h", "--help"},
			Description: "show this help and exit", Scope: CompletionShared,
		},
	} {
		if !containsCompletionOption(options, want) {
			t.Errorf("completion options omit %#v", want)
		}
	}
}

func TestCompletionOptionsAreIndependent(t *testing.T) {
	p := New()
	first := p.CompletionOptions()
	first[0].Names[0] = "changed"
	first[0].Description = "changed"
	for i := range first {
		if len(first[i].Values) > 0 {
			first[i].Values[0] = "changed"
			break
		}
	}
	second := p.CompletionOptions()
	if second[0].Names[0] == "changed" || second[0].Description == "changed" {
		t.Fatalf("CompletionOptions returned aliased metadata: %#v", second[0])
	}
	for _, option := range second {
		if len(option.Values) > 0 && option.Values[0] == "changed" {
			t.Fatalf("CompletionOptions returned aliased values: %#v", option)
		}
	}
}

func containsCompletionOption(
	options []CompletionOption,
	want CompletionOption,
) bool {
	for _, option := range options {
		if reflect.DeepEqual(option, want) {
			return true
		}
	}
	return false
}
