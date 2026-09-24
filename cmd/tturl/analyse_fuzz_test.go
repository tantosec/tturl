package main

import "testing"

func FuzzValidateAnalyseEvidence(f *testing.F) {
	f.Add([]byte{2, 6, 0, 0, 0, 0})
	f.Add([]byte{8, 11, 4, 7, 9, 255})
	f.Add([]byte{3, 7, 2, 1, 5, 100})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		width := int(data[0] % 11)
		cycles := int(data[1] % 14)
		evidence := analyseFuzzEvidence(width, cycles)
		if len(data) >= 6 && width > 0 && cycles > 0 {
			cycle := int(data[2]) % cycles
			shift := int(data[3]) % width
			trial := &evidence.Cycles[cycle].Trials[shift]
			switch data[4] % 8 {
			case 0:
				trial.Shift = int(data[5]) - 128
			case 1:
				trial.Arrangement[int(data[5])%width] = int(data[5])
			case 2:
				trial.Ranks[int(data[5])%width] = int(data[5])
			case 3:
				trial.Connection = int(data[5]) - 128
			case 4:
				trial.Statuses[int(data[5])%width] = int(data[5])
			case 5:
				trial.Arrangement = trial.Arrangement[:len(trial.Arrangement)-1]
			case 6:
				trial.Ranks = append(trial.Ranks, int(data[5]))
			case 7:
				trial.Statuses = trial.Statuses[:len(trial.Statuses)-1]
			}
		}
		_ = analyseOffline(evidence, analyseConfig{RandomisationSeed: 1})
	})
}

func analyseFuzzEvidence(width, cycles int) analyseEvidence {
	evidence := analyseEvidence{
		Width: width, Cycles: make([]analyseCycleEvidence, cycles),
	}
	for cycle := range cycles {
		base := make([]int, width)
		for position := range width {
			base[position] = (position + cycle) % width
		}
		for shift := range width {
			arrangement := make([]int, width)
			ranks := make([]int, width)
			statuses := make([]int, width)
			for position := range width {
				arrangement[position] = base[(position+shift)%width]
				ranks[position] = position
				statuses[position] = 200
			}
			evidence.Cycles[cycle].Trials = append(
				evidence.Cycles[cycle].Trials,
				analyseTrialEvidence{
					Connection: 0, Shift: shift, Arrangement: arrangement,
					Ranks: ranks, Statuses: statuses,
				},
			)
		}
	}
	return evidence
}
