package sflog

import "testing"

func TestIssueDetailHumanizesKnownErrors(t *testing.T) {
	tests := []struct {
		name string
		is   Issue
		want string
	}{
		{
			name: "not an archive",
			is:   Issue{Kind: IssueParseError, Err: ErrNotAnArchive},
			want: "not a recognized archive (signature mismatch)",
		},
		{
			name: "7z header eof",
			is:   Issue{Kind: IssueParseError, Err: errSevenZipEOF},
			want: "truncated or corrupt archive (header EOF)",
		},
		{
			name: "no ulp default",
			is:   Issue{Kind: IssueNoULP},
			want: "no credential files found",
		},
		{
			name: "password default",
			is:   Issue{Kind: IssuePasswordNotFound},
			want: "none of the candidate passwords worked",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IssueDetail(tc.is); got != tc.want {
				t.Fatalf("IssueDetail() = %q, want %q", got, tc.want)
			}
		})
	}
}

var errSevenZipEOF = errSevenZipHeaderEOF{}

type errSevenZipHeaderEOF struct{}

func (errSevenZipHeaderEOF) Error() string {
	return "sevenzip: error initialising: sevenzip: error reading header id: EOF"
}
