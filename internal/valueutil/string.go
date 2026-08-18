package valueutil

import "fmt"

// String is the shared permissive conversion used at JSON/JavaScript seams.
// Nil deliberately maps to the empty string instead of "<nil>".
func String(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
