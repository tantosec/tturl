package curlblocks

// CompletionScope identifies where an option is accepted in the request
// grammar. Global options are valid only in the preamble, shared options are
// valid in the preamble and every block, and block options are valid only
// after a --block separator.
type CompletionScope uint8

const (
	// CompletionGlobal marks an option valid only in the preamble.
	CompletionGlobal CompletionScope = iota
	// CompletionShared marks an option valid in the preamble and blocks.
	CompletionShared
	// CompletionBlock marks an option valid only inside blocks.
	CompletionBlock
)

// CompletionFileStyle describes how a value can contain a local file name.
// Shell generators use it to offer file candidates only after the grammar has
// unambiguously selected a file-backed form.
type CompletionFileStyle uint8

const (
	// CompletionNoFiles marks a value with no file-backed form.
	CompletionNoFiles CompletionFileStyle = iota
	// CompletionFiles marks a value that is itself a file name.
	CompletionFiles
	// CompletionAtFiles marks a value whose leading @ selects a file.
	CompletionAtFiles
	// CompletionURLencodeFiles marks curl's optional NAME@FILE form.
	CompletionURLencodeFiles
	// CompletionFormFiles marks curl's NAME=@FILE and NAME=<FILE forms.
	CompletionFormFiles
	// CompletionVaryFiles marks a NAME=@FILE vary source.
	CompletionVaryFiles
)

// CompletionOption describes one semantic option for shell completion.
type CompletionOption struct {
	// Names contains every accepted spelling, including leading dashes.
	// Equivalent long names and their shorthand stay together.
	Names []string

	// Argument is the semantic argument name shown by [Parser.Usage].
	Argument string

	// Description is the help text shown by [Parser.Usage].
	Description string

	// Values is the complete finite set of accepted values, if known.
	Values []string

	// FileStyle describes any file-backed form of the option's value.
	FileStyle CompletionFileStyle

	// Scope identifies where the option is accepted.
	Scope CompletionScope

	// TakesValue reports whether the option consumes a value.
	TakesValue bool

	// OpensBlock reports whether the option is the --block separator. The
	// separator accepts following positional URLs but consumes no value itself.
	OpensBlock bool
}

// CompletionOptions returns the parser's accepted options with their scopes,
// in registration order. It includes the request grammar's --block separator
// and the implicit -h/--help option. The result and its Names and Values slices
// are independent and may be modified by the caller.
func (p *Parser) CompletionOptions() []CompletionOption {
	var out []CompletionOption
	out = appendCompletionRegistry(out, p.Global, CompletionGlobal)
	out = appendCompletionRegistry(out, p.Shared, CompletionShared)
	out = append(out, completionOption(blockFlag, CompletionShared, false, true))
	out = appendCompletionRegistry(out, p.Block, CompletionBlock)
	out = append(out, completionOption(helpFlag, CompletionShared, false, false))
	return out
}

func appendCompletionRegistry(
	out []CompletionOption,
	r *Registry,
	scope CompletionScope,
) []CompletionOption {
	for _, opt := range r.options() {
		for _, entry := range opt.helpEntries() {
			out = append(out,
				completionOption(entry, scope, opt.takesArgument(), false))
		}
	}
	return out
}

func completionOption(
	entry helpEntry,
	scope CompletionScope,
	takesValue, opensBlock bool,
) CompletionOption {
	names := make([]string, 0, len(entry.flags)*2)
	for _, flag := range entry.flags {
		if flag.shorthand != "" {
			names = append(names, "-"+flag.shorthand)
		}
		names = append(names, "--"+flag.name)
	}
	return CompletionOption{
		Names:       names,
		Argument:    entry.argument,
		Description: entry.usage,
		Values:      append([]string(nil), entry.completionValues...),
		FileStyle:   entry.completionFiles,
		Scope:       scope,
		TakesValue:  takesValue,
		OpensBlock:  opensBlock,
	}
}
