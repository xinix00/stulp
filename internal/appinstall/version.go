package appinstall

// version.go — versies vergelijken. Puur rekenwerk op strings: geen netwerk,
// geen schijf, en dus op elk doel bruikbaar.
//
// Het pakket heette naar wat het niet meer doet: installeren vanaf GitHub is er
// uit. Een app komt binnen doordat iemand hem neerzet en zich meldt, en de versie
// die zich meldt IS de versie die er is. Wat er overblijft is dit vergelijk (voor
// het geval twee versies naast elkaar staan) en het opruimen van een bundel bij
// een uninstall (bundle.go).

import (
	"strconv"
	"strings"
)

// CompareVersions orders two semantic versions and reports whether both could
// be read. An unreadable version is answered with false rather than guessed:
// claiming an update that does not exist, or hiding one that does, is worse
// than saying the version could not be compared.
func CompareVersions(left, right string) (int, bool) {
	first, firstOK := parseVersion(left)
	second, secondOK := parseVersion(right)
	if !firstOK || !secondOK {
		return 0, false
	}
	for index := range first.numbers {
		if first.numbers[index] != second.numbers[index] {
			if first.numbers[index] < second.numbers[index] {
				return -1, true
			}
			return 1, true
		}
	}
	return comparePrerelease(first.prerelease, second.prerelease), true
}

type version struct {
	numbers    [3]uint64
	prerelease []string
}

// parseVersion accepts MAJOR.MINOR.PATCH with an optional prerelease, ignoring
// build metadata as the specification requires. All three numbers are demanded
// because shorter forms are ambiguous in practice: a calendar version like
// 2026-08-08 would otherwise read as 2026.0.0 with a prerelease and order
// wrongly against its own siblings.
func parseVersion(value string) (version, bool) {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(text, "v")
	if text == "" {
		return version{}, false
	}
	if plus := strings.IndexByte(text, '+'); plus >= 0 {
		text = text[:plus]
	}
	core, prerelease, hasPrerelease := strings.Cut(text, "-")
	parsed := version{}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	for index, part := range parts {
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version{}, false
		}
		parsed.numbers[index] = number
	}
	if !hasPrerelease {
		return parsed, true
	}
	if prerelease == "" {
		return version{}, false
	}
	for _, identifier := range strings.Split(prerelease, ".") {
		if identifier == "" || !isPrereleaseIdentifier(identifier) {
			return version{}, false
		}
		parsed.prerelease = append(parsed.prerelease, identifier)
	}
	return parsed, true
}

func isPrereleaseIdentifier(value string) bool {
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9',
			character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character == '-':
		default:
			return false
		}
	}
	return true
}

// comparePrerelease follows the precedence rules from the semantic versioning
// specification: a release outranks any prerelease, numeric identifiers compare
// as numbers and rank below alphanumeric ones, and a shorter run of equal
// identifiers ranks lower.
func comparePrerelease(left, right []string) int {
	switch {
	case len(left) == 0 && len(right) == 0:
		return 0
	case len(left) == 0:
		return 1
	case len(right) == 0:
		return -1
	}
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] == right[index] {
			continue
		}
		leftNumber, leftNumeric := strconv.ParseUint(left[index], 10, 64)
		rightNumber, rightNumeric := strconv.ParseUint(right[index], 10, 64)
		switch {
		case leftNumeric == nil && rightNumeric == nil:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftNumeric == nil:
			return -1
		case rightNumeric == nil:
			return 1
		case left[index] < right[index]:
			return -1
		default:
			return 1
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	default:
		return 0
	}
}
