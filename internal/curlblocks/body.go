package curlblocks

import "errors"

// resolveBody resolves the block's request body across scopes, returning the
// pieces for whichever single body kind is set. Body content accumulates
// preamble-into-block like other content, but a block's --reset-body drops the
// inherited body (-d/-F/--json) so it starts fresh; targeting, headers, and
// cookies still inherit as usual. At most one body kind may be set; mixing
// -d/--data, -F/--form, and --json is an error.
func resolveBody(pre, blk *segment) (data []dataPiece, form []formPart, jsonPieces []jsonPiece, err error) {
	// An empty stand-in for the preamble supplies no body under --reset-body.
	bodyPre := pre
	if fResetBody.value(blk) {
		bodyPre = &segment{}
	}
	form, err = resolveForm(bodyPre, blk)
	if err != nil {
		return nil, nil, nil, err
	}
	jsonPieces, err = resolveJSON(bodyPre, blk)
	if err != nil {
		return nil, nil, nil, err
	}
	data, err = resolveData(bodyPre, blk)
	if err != nil {
		return nil, nil, nil, err
	}
	kinds := 0
	for _, n := range []int{len(data), len(form), len(jsonPieces)} {
		if n > 0 {
			kinds++
		}
	}
	if kinds > 1 {
		return nil, nil, nil, errors.New("only one request body kind may be set: " +
			"-d/--data, -F/--form, or --json (a body set in the preamble applies to " +
			"every block; use --reset-body in a block to drop it)")
	}
	return data, form, jsonPieces, nil
}
