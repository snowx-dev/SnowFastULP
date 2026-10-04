package sflog

import (
	"testing"

	"github.com/nwaples/rardecode/v2"
)

func TestIssueDetail(t *testing.T) {
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
			name: "rar decoder out of data",
			is:   Issue{Kind: IssueParseError, Err: rardecode.ErrDecoderOutOfData},
			want: "truncated or corrupt member",
		},
		{
			name: "no credentials parsed",
			is:   Issue{Kind: IssueNoULP},
			want: "no credentials parsed",
		},
		{
			name: "password default",
			is:   Issue{Kind: IssuePasswordNotFound},
			want: "none of the candidate passwords worked",
		},
		{
			name: "mixed format",
			is:   Issue{Kind: IssueMixedFormat},
			want: "labeled blocks kept; valid label-less URL/login/password lines discarded",
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
