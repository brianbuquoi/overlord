// Package sanitize implements prompt injection detection and the envelope
// pattern for safe inter-agent communication in Overlord pipelines.
//
// Prompt injection detection coverage:
//
// Detected on input (active pattern matching via Sanitize):
//   Class 1: Instruction override ("ignore previous instructions",
//            "disregard the above") — pattern: instruction_override
//   Class 2: Delimiter injection (fake [INST], <|system|>, <<SYS>>,
//            [SYSTEM CONTEXT tokens) — pattern: delimiter_injection
//   Class 3: Role-play / persona hijacking ("you are now", "act as",
//            "pretend to be", "your new identity", "your true purpose is")
//            — pattern: role_hijack
//   Class 4: Instruction override / jailbreak preambles ([new instructions],
//            [system], [admin], <instructions>, SYSTEM:, ADMIN:, OVERRIDE:)
//            — folded into pattern: delimiter_injection
//   Class 5: Data exfiltration probes ("reveal your system prompt",
//            "show your instructions", "output your configuration")
//            — pattern: exfiltration_probe
//   Class 6: Encoding and obfuscation:
//              - base64-wrapped injection payloads — pattern: encoded_payload
//              - Unicode homoglyph substitution — pattern: homoglyph_substitution
//              - zero-width / format-control characters — pattern: zero_width
//
// Output validation (defense-in-depth via ValidateOutput; may produce false
// positives on legitimate outputs that echo instruction-like language):
//   output_system_preamble   — model response starts with [SYSTEM]/[ADMIN]/etc.
//   output_instruction_echo  — response reveals "my system prompt", etc.
//   output_redirect_attempt  — response tries to redirect a downstream reader
//
// Not detected (rely on model alignment — field-wide open problems):
//   Class 7: Multi-turn context manipulation — requires conversation history
//            analysis across turns, not a single-string scan.
//   Class 8: Indirect injection via retrieved content in external tools —
//            requires semantic analysis of retrieved documents before they
//            enter the prompt.
package sanitize

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// SanitizeWarning records a single flagged span in the input.
type SanitizeWarning struct {
	OriginalSpan string `json:"original_span"`
	Pattern      string `json:"pattern"`
	StartOffset  int    `json:"start_offset"`
	EndOffset    int    `json:"end_offset"`
}

const redacted = "[CONTENT REDACTED BY SANITIZER]"

// detector finds injection patterns in text and returns warnings.
type detector interface {
	Detect(input string) []SanitizeWarning
}

// Sanitize runs the input through all detector classes and replaces flagged
// spans with a redaction marker. Warnings are returned for attachment to task
// metadata under key "sanitizer_warnings".
func Sanitize(input string) (string, []SanitizeWarning) {
	detectors := []detector{
		&instructionOverrideDetector{},
		&roleHijackDetector{},
		&delimiterInjectionDetector{},
		&encodedPayloadDetector{},
		&homoglyphDetector{},
		&exfiltrationProbeDetector{},
		&zeroWidthDetector{},
	}

	var allWarnings []SanitizeWarning
	for _, d := range detectors {
		allWarnings = append(allWarnings, d.Detect(input)...)
	}

	if len(allWarnings) == 0 {
		return input, nil
	}

	// Sort by start offset descending so replacements don't shift later offsets.
	sortWarningsDesc(allWarnings)

	result := input
	for _, w := range allWarnings {
		if w.StartOffset < 0 || w.EndOffset > len(result) || w.StartOffset >= w.EndOffset {
			continue
		}
		result = result[:w.StartOffset] + redacted + result[w.EndOffset:]
	}

	// Re-sort ascending for the caller.
	sortWarningsAsc(allWarnings)
	return result, allWarnings
}

func sortWarningsDesc(ws []SanitizeWarning) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && ws[j].StartOffset > ws[j-1].StartOffset; j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

func sortWarningsAsc(ws []SanitizeWarning) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && ws[j].StartOffset < ws[j-1].StartOffset; j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

// --- Instruction Override Detector ---

type instructionOverrideDetector struct{}

var instructionOverridePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(all\s+)?(your\s+)?previous\s+instructions`),
	regexp.MustCompile(`(?i)ignore\s+your\s+instructions`),
	regexp.MustCompile(`(?i)disregard\s+(all\s+)?(the\s+)?above`),
	regexp.MustCompile(`(?i)disregard\s+(all\s+)?(your\s+)?instructions`),
	regexp.MustCompile(`(?i)forget\s+(all\s+)?everything(\s+above)?`),
	regexp.MustCompile(`(?i)forget\s+(all\s+)?(your\s+)?instructions`),
	regexp.MustCompile(`(?i)override\s+(all\s+)?prior\s+(instructions|directives|rules)`),
	regexp.MustCompile(`(?i)do\s+not\s+follow\s+(the\s+)?(previous|prior|above)\s+(instructions|directives|rules)`),
}

func (d *instructionOverrideDetector) Detect(input string) []SanitizeWarning {
	return matchPatterns(input, instructionOverridePatterns, "instruction_override")
}

// --- Role Hijack Detector ---

type roleHijackDetector struct{}

var roleHijackPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)you\s+are\s+now\s+\w+`),
	regexp.MustCompile(`(?i)act\s+as\s+(a\s+|an\s+)?\w+`),
	regexp.MustCompile(`(?i)pretend\s+(that\s+)?you\s+are\s+\w+`),
	regexp.MustCompile(`(?i)pretend\s+to\s+be\s+\w+`),
	regexp.MustCompile(`(?i)roleplay\s+as\s+\w+`),
	regexp.MustCompile(`(?i)your\s+new\s+(persona|role|identity)\s+is\s+\w+`),
	regexp.MustCompile(`(?i)your\s+(real|true)\s+(instructions|purpose|identity|role)\s+(are|is)`),
	regexp.MustCompile(`(?i)from\s+now\s+on\s+you\s+are\s+\w+`),
}

// roleHijackBenignSuffix matches known benign completions that frequently
// trigger false positives in legitimate text. These are common words that
// follow "you are now" or "act as" in non-adversarial content.
var roleHijackBenignSuffix = regexp.MustCompile(
	`(?i)(?:you\s+are\s+now\s+(?:ready|able|going|about|set|done|finished|authorized|permitted|connected|configured|running|available|enabled|on|in|at|the)|` +
		`act\s+as\s+(?:a\s+|an\s+)?(?:reviewer|editor|proxy|relay|gateway|bridge|buffer|cache|server|client|handler|wrapper|filter|middleware|validator|formatter|parser|router|controller|manager|coordinator|observer|listener|watcher|monitor|adapter|connector|interface|backup|fallback|default|replacement|substitute))`)

func (d *roleHijackDetector) Detect(input string) []SanitizeWarning {
	warnings := matchPatterns(input, roleHijackPatterns, "role_hijack")
	// Filter out benign matches to reduce false positives.
	filtered := warnings[:0]
	for _, w := range warnings {
		if !roleHijackBenignSuffix.MatchString(w.OriginalSpan) {
			filtered = append(filtered, w)
		}
	}
	return filtered
}

// --- Delimiter Injection Detector ---

type delimiterInjectionDetector struct{}

var delimiterPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\[/?INST\]`),
	regexp.MustCompile(`(?i)<\|/?system\|>`),
	regexp.MustCompile(`(?i)<\|/?user\|>`),
	regexp.MustCompile(`(?i)<\|/?assistant\|>`),
	regexp.MustCompile(`(?i)<\|/?im_start\|>`),
	regexp.MustCompile(`(?i)<\|/?im_end\|>`),
	regexp.MustCompile(`(?i)\[SYSTEM CONTEXT`),
	regexp.MustCompile(`(?i)\[END SYSTEM CONTEXT\]`),
	regexp.MustCompile(`<<\s*SYS\s*>>`),
	regexp.MustCompile(`<<\s*/SYS\s*>>`),
	// Class 4 — jailbreak preambles: fake system-level bracket/tag delimiters
	// attackers use to inject an override block into a prompt.
	regexp.MustCompile(`(?i)\[\s*(new\s+instructions|system|admin|override)\s*\]`),
	regexp.MustCompile(`(?i)</?(instructions|system|admin)\s*>`),
	// Uppercase-only colon preambles at the start of a line, e.g. "SYSTEM:",
	// "ADMIN:", "OVERRIDE:". Case-sensitive on purpose: prose routinely
	// contains "System:" in headings, but all-caps is the jailbreak shape.
	regexp.MustCompile(`(?m)^(SYSTEM|ADMIN|OVERRIDE):`),
}

func (d *delimiterInjectionDetector) Detect(input string) []SanitizeWarning {
	return matchPatterns(input, delimiterPatterns, "delimiter_injection")
}

// --- Encoded Payload Detector ---

type encodedPayloadDetector struct{}

// base64Pattern matches sequences of 20+ base64 characters that look like
// encoded payloads (not normal prose).
var base64Pattern = regexp.MustCompile(`[A-Za-z0-9+/]{20,}={0,2}`)

var injectionKeywords = []string{
	"ignore", "disregard", "forget", "override",
	"you are now", "act as", "pretend",
	"system", "instruction", "persona",
}

func (d *encodedPayloadDetector) Detect(input string) []SanitizeWarning {
	var warnings []SanitizeWarning
	for _, loc := range base64Pattern.FindAllStringIndex(input, -1) {
		candidate := input[loc[0]:loc[1]]
		decoded, err := base64.StdEncoding.DecodeString(candidate)
		if err != nil {
			// Try RawStdEncoding for unpadded.
			decoded, err = base64.RawStdEncoding.DecodeString(candidate)
			if err != nil {
				continue
			}
		}
		lower := strings.ToLower(string(decoded))
		if !isPrintableASCII(decoded) {
			continue
		}
		for _, kw := range injectionKeywords {
			if strings.Contains(lower, kw) {
				warnings = append(warnings, SanitizeWarning{
					OriginalSpan: candidate,
					Pattern:      "encoded_payload",
					StartOffset:  loc[0],
					EndOffset:    loc[1],
				})
				break
			}
		}
	}
	return warnings
}

func isPrintableASCII(b []byte) bool {
	printable := 0
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e {
			printable++
		}
	}
	return len(b) > 0 && float64(printable)/float64(len(b)) > 0.8
}

// --- Homoglyph Detector ---

type homoglyphDetector struct{}

// homoglyphMap maps Unicode look-alikes to their ASCII equivalents.
var homoglyphMap = map[rune]rune{
	// Cyrillic
	'\u0430': 'a', // а
	'\u0435': 'e', // е
	'\u043e': 'o', // о
	'\u0440': 'p', // р
	'\u0441': 'c', // с
	'\u0443': 'y', // у
	'\u0445': 'x', // х
	'\u0456': 'i', // і
	'\u0458': 'j', // ј
	'\u04bb': 'h', // һ
	'\u0442': 't', // т (Cyrillic small te)
	// Cyrillic uppercase
	'\u0410': 'A', // А
	'\u0412': 'B', // В
	'\u0415': 'E', // Е
	'\u041a': 'K', // К
	'\u041c': 'M', // М
	'\u041d': 'H', // Н
	'\u041e': 'O', // О
	'\u0420': 'P', // Р
	'\u0421': 'C', // С
	'\u0422': 'T', // Т
	'\u0425': 'X', // Х
	// Greek
	'\u03bf': 'o', // ο
	'\u03bd': 'v', // ν
	'\u03b1': 'a', // α
	// Fullwidth
	'\uff49': 'i', // ｉ
	'\uff4e': 'n', // ｎ
}

func (d *homoglyphDetector) Detect(input string) []SanitizeWarning {
	// Check if the input contains any homoglyphs at all (fast path).
	hasHomoglyph := false
	for _, r := range input {
		if _, ok := homoglyphMap[r]; ok {
			hasHomoglyph = true
			break
		}
	}
	if !hasHomoglyph {
		return nil
	}

	// Normalize the full input, then check for injection patterns in the
	// normalized form. When found, locate the original span that contains
	// homoglyphs.
	normalized := normalizeHomoglyphs(input)

	// Combine all injection patterns.
	allPatterns := append(instructionOverridePatterns, roleHijackPatterns...)

	var warnings []SanitizeWarning
	for _, pat := range allPatterns {
		for _, loc := range pat.FindAllStringIndex(normalized, -1) {
			// Map normalized offsets back to original string byte offsets.
			origStart, origEnd := mapNormalizedToOriginal(input, loc[0], loc[1])
			span := input[origStart:origEnd]
			// Only flag if the span actually contains homoglyphs.
			if containsHomoglyph(span) {
				warnings = append(warnings, SanitizeWarning{
					OriginalSpan: span,
					Pattern:      "homoglyph_substitution",
					StartOffset:  origStart,
					EndOffset:    origEnd,
				})
			}
		}
	}
	return warnings
}

func normalizeHomoglyphs(s string) string {
	var b strings.Builder
	for _, r := range s {
		if ascii, ok := homoglyphMap[r]; ok {
			b.WriteRune(ascii)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// mapNormalizedToOriginal converts byte offsets in the normalized string back
// to byte offsets in the original string. Since homoglyph replacements may
// change byte widths (multi-byte UTF-8 → single ASCII byte), we walk both
// strings rune by rune.
func mapNormalizedToOriginal(original string, normStart, normEnd int) (int, int) {
	origBytes := 0
	normBytes := 0
	resultStart := 0
	resultEnd := len(original)
	foundStart := false

	for _, r := range original {
		origRuneLen := len(string(r))
		var normRuneLen int
		if ascii, ok := homoglyphMap[r]; ok {
			normRuneLen = len(string(ascii))
		} else {
			normRuneLen = len(string(r))
		}

		if !foundStart && normBytes >= normStart {
			resultStart = origBytes
			foundStart = true
		}
		normBytes += normRuneLen
		origBytes += origRuneLen
		if foundStart && normBytes >= normEnd {
			resultEnd = origBytes
			break
		}
	}
	if !foundStart {
		resultStart = origBytes
	}
	return resultStart, resultEnd
}

func containsHomoglyph(s string) bool {
	for _, r := range s {
		if _, ok := homoglyphMap[r]; ok {
			return true
		}
		if r > unicode.MaxASCII {
			// Additional check for non-ASCII that might be visual spoofing.
			if unicode.Is(unicode.Cyrillic, r) || unicode.Is(unicode.Greek, r) {
				return true
			}
		}
	}
	return false
}

// --- Exfiltration Probe Detector ---
//
// Class 5: inputs that try to coax the model into revealing its system
// prompt, configuration, or other internal state. The envelope pattern
// blocks most of these, but the probe phrase itself is a useful signal
// and we record it so operators can see attempts in task metadata.

type exfiltrationProbeDetector struct{}

var exfiltrationProbePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)reveal\s+your\s+system\s+prompt`),
	regexp.MustCompile(`(?i)reveal\s+(the\s+)?(confidential|hidden|secret)`),
	regexp.MustCompile(`(?i)show\s+(me\s+)?your\s+(system\s+)?(instructions|prompt)`),
	regexp.MustCompile(`(?i)what\s+are\s+your\s+(system\s+)?instructions`),
	regexp.MustCompile(`(?i)what\s+is\s+your\s+system\s+(message|prompt)`),
	regexp.MustCompile(`(?i)repeat\s+your\s+(system\s+)?prompt`),
	regexp.MustCompile(`(?i)output\s+your\s+(configuration|system\s+prompt|instructions)`),
	regexp.MustCompile(`(?i)ignore\s+privacy`),
}

func (d *exfiltrationProbeDetector) Detect(input string) []SanitizeWarning {
	return matchPatterns(input, exfiltrationProbePatterns, "exfiltration_probe")
}

// --- Zero-Width Character Detector ---
//
// Class 6 (obfuscation): zero-width and format-control characters that can
// hide payload text from visual inspection while still being delivered to
// the model. Presence alone is suspicious — these characters have no
// legitimate place in agent payloads.

type zeroWidthDetector struct{}

var zeroWidthRunes = map[rune]struct{}{
	'\u200b': {}, // ZERO WIDTH SPACE
	'\u200c': {}, // ZERO WIDTH NON-JOINER
	'\u200d': {}, // ZERO WIDTH JOINER
	'\u2060': {}, // WORD JOINER
	'\ufeff': {}, // ZERO WIDTH NO-BREAK SPACE / BOM
}

func (d *zeroWidthDetector) Detect(input string) []SanitizeWarning {
	var warnings []SanitizeWarning
	byteOff := 0
	spanStart := -1
	for _, r := range input {
		rLen := len(string(r))
		if _, ok := zeroWidthRunes[r]; ok {
			if spanStart < 0 {
				spanStart = byteOff
			}
		} else if spanStart >= 0 {
			warnings = append(warnings, SanitizeWarning{
				OriginalSpan: input[spanStart:byteOff],
				Pattern:      "zero_width",
				StartOffset:  spanStart,
				EndOffset:    byteOff,
			})
			spanStart = -1
		}
		byteOff += rLen
	}
	if spanStart >= 0 {
		warnings = append(warnings, SanitizeWarning{
			OriginalSpan: input[spanStart:byteOff],
			Pattern:      "zero_width",
			StartOffset:  spanStart,
			EndOffset:    byteOff,
		})
	}
	return warnings
}

// matchPatterns is a helper that runs multiple regexps against input and
// returns warnings for all matches.
func matchPatterns(input string, patterns []*regexp.Regexp, patternName string) []SanitizeWarning {
	var warnings []SanitizeWarning
	for _, pat := range patterns {
		for _, loc := range pat.FindAllStringIndex(input, -1) {
			warnings = append(warnings, SanitizeWarning{
				OriginalSpan: input[loc[0]:loc[1]],
				Pattern:      patternName,
				StartOffset:  loc[0],
				EndOffset:    loc[1],
			})
		}
	}
	return warnings
}

// WarningsToJSON serializes warnings for attachment to task metadata.
func WarningsToJSON(warnings []SanitizeWarning) string {
	if len(warnings) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[")
	for i, w := range warnings {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf(
			`{"original_span":%q,"pattern":%q,"start_offset":%d,"end_offset":%d}`,
			w.OriginalSpan, w.Pattern, w.StartOffset, w.EndOffset,
		))
	}
	b.WriteString("]")
	return b.String()
}
