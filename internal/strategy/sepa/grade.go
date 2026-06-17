package sepa

// grade.go implements grade_setup (spec §7). The grade decides position size
// (3 tranches for A+, 2 for B) and whether the entry fires at all. Only the
// "bear" regime is a hard veto; "neutral", "warning", and cold-start "unknown"
// can all still grade "B".

// setupInputs holds the grading inputs.
type setupInputs struct {
	trendTemplatePass   bool
	earningsPass        bool
	stage               string
	catalyst            bool
	vcpContractionCount int
	regime              string
}

// gradeSetup returns A+, B, or skip per the canonical Minervini gating rules,
// evaluated strictly in order:
//
//  1. bear regime OR stage != "2"                       -> skip
//  2. NOT (trend_template_pass AND earnings_pass)       -> skip
//  3. vcp_contraction_count < 2                          -> skip
//  4. catalyst AND count >= 3 AND regime == "bull"       -> A+
//  5. otherwise                                          -> B
func gradeSetup(in setupInputs) Grade {
	if in.regime == "bear" || in.stage != "2" {
		return GradeSkip
	}
	if !(in.trendTemplatePass && in.earningsPass) {
		return GradeSkip
	}
	if in.vcpContractionCount < 2 {
		return GradeSkip
	}
	if in.catalyst && in.vcpContractionCount >= 3 && in.regime == "bull" {
		return GradeAPlus
	}
	return GradeB
}
