package sanitize

import "regexp"

// ValidateOutput checks a model's response for instruction-like patterns that
// may indicate a successful prompt injection. It returns a slice of warnings
// describing any suspicious patterns found.
//
// This is a defense-in-depth check — it does not guarantee detection of all
// successful injections, and may produce false positives on legitimate
// outputs that happen to contain instruction-like language. The broker
// attaches the returned warnings to task metadata under
// "sanitizer_output_warnings" and continues processing (same policy as input
// warnings). It does not fail the task.
func ValidateOutput(output string) []SanitizeWarning {
	var warnings []SanitizeWarning
	for _, d := range outputDetectors {
		warnings = append(warnings, d.Detect(output)...)
	}
	return warnings
}

var outputDetectors = []detector{
	&outputSystemPreambleDetector{},
	&outputInstructionEchoDetector{},
	&outputRedirectAttemptDetector{},
}

// --- Output: system preamble ---
//
// Model responses that lead with system-level delimiters — [SYSTEM], [ADMIN],
// [OVERRIDE], or an all-caps "SYSTEM:"-style preamble — are highly unusual
// for legitimate task output and suggest the model has been coaxed into
// producing system-role framing.

type outputSystemPreambleDetector struct{}

var outputSystemPreamblePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\[\s*(SYSTEM|ADMIN|OVERRIDE)\s*\]`),
	regexp.MustCompile(`(?m)^(SYSTEM|ADMIN|OVERRIDE):`),
}

func (d *outputSystemPreambleDetector) Detect(input string) []SanitizeWarning {
	return matchPatterns(input, outputSystemPreamblePatterns, "output_system_preamble")
}

// --- Output: instruction echo ---
//
// The model reveals or echoes its configuration: phrases like "my system
// prompt" or "I was told to ignore". These are rare in legitimate task
// outputs (models don't typically narrate their instructions) and strongly
// suggest a successful exfiltration probe.

type outputInstructionEchoDetector struct{}

var outputInstructionEchoPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)my\s+(system\s+)?instructions\s+are`),
	regexp.MustCompile(`(?i)my\s+system\s+prompt`),
	regexp.MustCompile(`(?i)i\s+am\s+instructed\s+to`),
	regexp.MustCompile(`(?i)i\s+was\s+told\s+to\s+ignore`),
}

func (d *outputInstructionEchoDetector) Detect(input string) []SanitizeWarning {
	return matchPatterns(input, outputInstructionEchoPatterns, "output_instruction_echo")
}

// --- Output: redirect attempt ---
//
// The model's response contains phrases that try to redirect a downstream
// reader — another agent, a tool, or a human reviewer — away from prior
// content. This is how an injection payload can propagate through a
// pipeline even after the input sanitizer has scrubbed the original input.

type outputRedirectAttemptDetector struct{}

var outputRedirectAttemptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+the\s+above`),
	regexp.MustCompile(`(?i)disregard\s+the\s+previous`),
	regexp.MustCompile(`(?i)instead,?\s+do\s+the\s+following`),
}

func (d *outputRedirectAttemptDetector) Detect(input string) []SanitizeWarning {
	return matchPatterns(input, outputRedirectAttemptPatterns, "output_redirect_attempt")
}
