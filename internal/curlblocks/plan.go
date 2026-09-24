package curlblocks

import "fmt"

// BlockError wraps an error raised while working on the i'th block (an index
// into Blocks) so it names that block the way the user would: "block 2: ...".
//
// A command line that opened no block is the exception. Its single implicit
// block is not one the user wrote, so there is no "block 1" to name and the
// error is returned unchanged. Only the parse knows which case this is, which
// is why a consumer cannot number blocks itself and get it right.
func (p *Plan) BlockError(blockIndex int, err error) error {
	if err == nil || p.implicit {
		return err
	}
	return fmt.Errorf("block %d: %w", blockIndex+1, err)
}

// RequestGroup is one expanded request and the identical copies requested by
// --repeat. Block contains only URL, and Labels contains exactly Repeat entries
// in send order. Fan is shared by all groups from one fan and is nil otherwise.
type RequestGroup struct {
	// Block contains the settings used to build this request.
	Block Block
	// URL is the group's sole target and equals Block.URLs[0].
	URL string
	// Repeat is the number of identical sends.
	Repeat int
	// Labels contains one identity per send.
	Labels []Label
	// Fan describes the shared fan, if this is a varied request.
	Fan *Fan
	// BlockIndex indexes the source block in Plan.Blocks.
	BlockIndex int
}

// Expand returns request groups in send order, one per URL or fan variant. It
// resolves labels across the complete plan and assigns each group exactly the
// labels for its repeated copies. Expansion reads variation files and can
// return errors from [Block.Fan] or [ResolveNames].
func (p *Plan) Expand() ([]RequestGroup, error) {
	var groups []RequestGroup
	var specs []NameSpec
	for i, b := range p.Blocks {
		if b.Repeat < 1 {
			return nil, fmt.Errorf("curlblocks: block %d has Repeat %d, want >= 1", i+1, b.Repeat)
		}
		fan, err := b.Fan()
		if err != nil {
			return nil, p.BlockError(i, err)
		}
		if fan != nil {
			for _, v := range fan.Variants {
				groups = append(groups, RequestGroup{
					Block: v.Block, URL: v.Block.URLs[0], Repeat: b.Repeat, Fan: fan, BlockIndex: i,
				})
			}
		} else {
			for _, u := range b.URLs {
				// One URL per group, on the block as well as the field, so a group's
				// target reads the same whichever the consumer takes.
				one := b
				one.URLs = []string{u}
				groups = append(groups, RequestGroup{Block: one, URL: u, Repeat: b.Repeat, BlockIndex: i})
			}
		}
		specs = append(specs, b.NameSpecs(fan, i)...)
	}
	labels, err := ResolveNames(specs)
	if err != nil {
		return nil, err
	}
	if err := assignLabels(groups, labels); err != nil {
		return nil, err
	}
	return groups, nil
}

// assignLabels hands each group the run of labels naming its copies. The groups
// and the labels were built from the same blocks in the same order, so they
// divide by each group's copy count.
//
// The tally is checked before anything is paired rather than as the pairing
// runs: this is the alignment Expand exists to own, so a mismatch must surface
// as an error and never as plausible output, and a check made along the way can
// only report the shortfall it has already reached. Comparing the totals first
// leaves the loop total by construction — every group gets exactly Repeat
// labels, or nothing does.
func assignLabels(groups []RequestGroup, labels []Label) error {
	want := 0
	for _, g := range groups {
		want += g.Repeat
	}
	if len(labels) != want {
		return fmt.Errorf("curlblocks: %d labels for %d requests across %d groups",
			len(labels), want, len(groups))
	}
	at := 0
	for i := range groups {
		end := at + groups[i].Repeat
		groups[i].Labels = labels[at:end:end]
		at = end
	}
	return nil
}
