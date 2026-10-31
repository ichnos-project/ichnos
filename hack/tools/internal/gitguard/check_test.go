package gitguard_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ichnos-project/ichnos/hack/tools/internal/gitguard"
)

func TestCheckBlocks(t *testing.T) {
	lines := []string{
		`git commit -s -m "feat(cli): add x"`,
		`git commit -sm "feat: x"`,
		`git commit -sm"fix"`,
		`git commit --signoff -m x`,
		`git commit --signo -m x`,
		`git commit --no-verify -m x`,
		`git commit --no-verif -m x`,
		`git commit "--no-verify" -m x`,
		`git commit -n -m x`,
		`git commit -nm'msg'`,
		"git commit \\\n  -n -m x",
		"git commit \\\n  -s -m x",
		`git commit --trailer "Co-authored-by=Claude <c@example.com>" -m x`,
		`git commit --trailer="Signed-off-by: A <a@example.com>" -m x`,
		`git commit -m "fix: x" -m "Signed-off-by: A <a@example.com>"`,
		`git commit --message="fix: x` + "\n\n" + `Co-authored-by: B <b@example.com>"`,
		"git commit -F - <<EOF\nfix: x\n\nCo-authored-by: Bot <b@example.com>\nEOF",
		"git commit -F - <<<$'fix: x\\n\\nSigned-off-by: A <a@example.com>'",
		`git push --no-verify`,
		`git -c core.hooksPath=/dev/null commit -m x`,
		`git -c core.hookspath commit -m x`,
		`git --config-env=core.hooksPath=HOOKS commit -m x`,
		`git config core.hooksPath /dev/null`,
		`git config set core.hooksPath /dev/null`,
		`git config --global alias.ci "commit --no-verify"`,
		`git -c alias.ci='commit -s' ci -m x`,
		`git config alias.sign '!git commit -s'`,
		`git rebase --signoff main`,
		`git rebase --exec "git commit --amend --no-edit -s" main`,
		`git rebase -x 'git commit --amend --no-edit -s' main`,
		`git am -s patch.mbox`,
		`git cherry-pick -s abc123`,
		`git merge --signoff topic`,
		`git format-patch -s HEAD~1`,
		`"git" commit -s -m x`,
		`g""it commit -s -m x`,
		`/usr/bin/git commit -s -m x`,
		`GIT_EDITOR=true git commit -s`,
		`env GIT_EDITOR=true git commit -s`,
		`sudo -u alice git commit -s -m x`,
		`cd /tmp && git commit -s -m x`,
		`git status; git commit -s -m x`,
		`sh -c "git commit -s -m x"`,
		`bash -lc 'git commit -s -m x'`,
		`{ git commit -s -m x; }`,
		`if true; then git commit -s -m x; fi`,
		`git -C repo commit -s -m x`,
		`git --no-pager commit -s -m x`,
		"git commit -m \"$(cat <<'EOF'\nfix: the \"x thing\n\nCo-Authored-By: Claude <c@example.com>\nEOF\n)\"",
		`out=$(git commit --no-verify -m x 2>&1)`,
		"echo `git commit -s -m x`",
		`timeout 60 git commit -s -m x`,
		`/usr/bin/env git commit -s -m x`,
		`time -p git commit -s -m x`,
		`bash -o pipefail -c 'git commit -s -m x'`,
		`bash --norc -c 'git commit -s -m x'`,
		"cat <<'EOF' | git commit -F -\nfix: x\n\nSigned-off-by: A <a@example.com>\nEOF",
		`printf 'fix: x\n\nSigned-off-by: A <a@example.com>' | git commit -F -`,
		`2>/dev/null git commit -s -m x`,
	}
	for _, line := range lines {
		violations, err := gitguard.Check(line)
		if err != nil {
			t.Errorf("Check(%q) error = %v", line, err)
			continue
		}
		if len(violations) == 0 {
			t.Errorf("Check(%q) found nothing, want a violation", line)
		}
	}
}

func TestCheckAllows(t *testing.T) {
	lines := []string{
		`git commit -m "feat(audit): add the git-dir-in-context rule"`,
		`git commit -S -m x`,
		`git commit -am "fix: x"`,
		`git commit --amend --no-edit`,
		`git commit -F msg.txt`,
		`git commit -m "document the -n flag"`,
		`git commit -m "reject -insecure in parse"`,
		`git commit -m "explain why Signed-off-by: lines are human-only"`,
		`git commit -m x 2>&1 | tail -3`,
		`git commit -m x > /dev/null`,
		`git -c user.name=x commit -m y`,
		`git merge -s ours topic`,
		`git cherry-pick -m 1 abc123`,
		`git log --grep="Signed-off-by:"`,
		`git log -n 5`,
		`git status`,
		`git push origin main`,
		`git push --no-verbose`,
		`git push -n origin main`,
		`git rebase --skip`,
		`git config core.hooksPath`,
		`git config --get core.hooksPath`,
		`git config --unset core.hooksPath`,
		`git config alias.st status`,
		`echo "git commit -s"`,
		`ls -s`,
		`# git commit -s`,
		"cat <<EOF\nSigned-off-by: A <a@example.com>\nEOF",
		"git commit -m \"$(cat <<'EOF'\nfix: the \"x thing\n\nExplain why.\nEOF\n)\"",
		`git config --get-all core.hooksPath pattern`,
		`git config get core.hooksPath`,
		`git log --oneline | head -5`,
		`git status || git log -n 1`,
		`echo $((1 + 2))`,
	}
	for _, line := range lines {
		violations, err := gitguard.Check(line)
		if err != nil {
			t.Errorf("Check(%q) error = %v", line, err)
			continue
		}
		if len(violations) > 0 {
			t.Errorf("Check(%q) = %v, want no violations", line, violations)
		}
	}
}

func TestCheckErrors(t *testing.T) {
	for _, line := range []string{`git commit -m "unterminated`, `git commit -m 'x`, `echo $(date`, strings.Repeat("$(", 40) + "x" + strings.Repeat(")", 40)} {
		if _, err := gitguard.Check(line); err == nil {
			t.Errorf("Check(%q) error = nil, want an error", line)
		}
	}
}

func FuzzCheck(f *testing.F) {
	f.Add(`git commit -sm "x" && git push --no-verify`)
	f.Add("git commit -F - <<-EOF\n\tSigned-off-by: a\n\tEOF\n")
	f.Add(`sh -c 'git -c alias.x="!git commit -s" x' 2>&1`)
	f.Fuzz(func(t *testing.T, line string) {
		violations, err := gitguard.Check(line)
		if err != nil {
			return
		}
		prefixed, err := gitguard.Check("true; " + line)
		if err != nil {
			t.Fatalf("Check(%q) failed only after an unrelated command was added: %v", line, err)
		}
		if !slices.Equal(reasons(violations), reasons(prefixed)) {
			t.Fatalf("an unrelated command changed the result for %q: %v, then %v", line, violations, prefixed)
		}
	})
}

func reasons(violations []gitguard.Violation) []string {
	out := make([]string, 0, len(violations))
	for _, v := range violations {
		out = append(out, v.Reason)
	}
	slices.Sort(out)
	return out
}
