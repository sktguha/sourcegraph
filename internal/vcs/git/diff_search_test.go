	"context"
	"strings"
	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()
	<-expiredCtx.Done()

		name       string
		ctx        context.Context
		opt        RawLogDiffSearchOptions
		want       []*LogCommitSearchResult
		incomplete bool
		errorS     string
	}, {
		name: "refglob",
		opt: RawLogDiffSearchOptions{
			Query: TextSearchOptions{Pattern: "root"},
			Diff:  true,
			Args:  []string{"--glob=refs/tags/*"},
		},
		want: []*LogCommitSearchResult{{
			Commit: Commit{
				ID:        "ce72ece27fd5c8180cfbc1c412021d32fd1cda0d",
				Author:    Signature{Name: "a", Email: "a@a.com", Date: MustParseTime(time.RFC3339, "2006-01-02T15:04:05Z")},
				Committer: &Signature{Name: "a", Email: "a@a.com", Date: MustParseTime(time.RFC3339, "2006-01-02T15:04:05Z")},
				Message:   "root",
			},
			Refs:       []string{"refs/heads/master", "refs/tags/mytag"},
			SourceRefs: []string{"refs/tags/mytag"},
			Diff:       &RawDiff{Raw: "diff --git a/f b/f\nnew file mode 100644\nindex 0000000..d8649da\n--- /dev/null\n+++ b/f\n@@ -0,0 +1,1 @@\n+root\n"},
		}},
	}, {
		name: "deadline",
		ctx:  expiredCtx,
		opt: RawLogDiffSearchOptions{
			Query: TextSearchOptions{Pattern: "root"},
			Diff:  true,
		},
		incomplete: true,
	}, {
		name: "not found",
		opt: RawLogDiffSearchOptions{
			Query: TextSearchOptions{Pattern: "root"},
			Diff:  true,
			Args:  []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		errorS: "fatal: bad object aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ctx := test.ctx
			if ctx == nil {
				ctx = context.Background()
			}

				if test.errorS == "" {
					t.Fatal(err)
				} else if !strings.Contains(err.Error(), test.errorS) {
					t.Fatalf("error should contain %q: %v", test.errorS, err)
				}
				return
			} else if test.errorS != "" {
				t.Fatal("expected error")

			if complete == test.incomplete {
				t.Fatalf("complete is %v", complete)
func TestRepository_RawLogDiffSearch_empty(t *testing.T) {
		"commit": {
		"repo": {
			repo: MakeGitRepository(t),
			want: map[*RawLogDiffSearchOptions][]*LogCommitSearchResult{
				{
					Paths: PathOptions{IncludePatterns: []string{"/xyz.txt"}, IsRegExp: true},
				}: nil, // want no matches
			},
		},
			results, complete, err := RawLogDiffSearch(context.Background(), test.repo, *opt)