// Package plural renders count nouns in singular/plural UI strings.
package plural

// Noun returns singular when n is exactly one, plural otherwise. Bare noun
// shape — call sites render the count themselves:
//
//	fmt.Sprintf("%d %s", n, plural.Noun(n, "file", "files"))
func Noun(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
