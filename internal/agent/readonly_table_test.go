package agent

import "testing"

// readonly_table_test.go — one table with every command from the v0.8.8
// classifier fixes (items 1-3) and the discovery report (R1-R8, G3). Each row
// states the expected verdict for both classifiers. The two must agree on
// the destructive/read-only split: a command is never both, and a row that is
// read-only is never destructive.

type classifyRow struct {
	cmd string
	ro  bool // IsReadOnlyShell
	d   bool // IsDestructiveShell
	why string
}

var classifyTable = []classifyRow{
	// R1: a leading redirect hides the command word. Destructive now.
	{">/dev/null rm -rf x", false, true, "R1 leading redirect hides rm"},
	{"> /dev/null rm -rf x", false, true, "R1 spaced redirect hides rm"},
	{"2>&1 rm x", false, true, "R1 fd dup then rm"},
	{"2>/dev/null rm x", false, true, "R1 stderr discard then rm"},
	{"echo hi; 2>/dev/null rm x", false, true, "R1 redirect after separator"},
	{"</dev/null rm -rf x", false, true, "R1 leading input redirect"},
	{"X=1 >/dev/null rm -rf x", false, true, "R1 assignment then redirect"},
	// R2: ">&1out.txt" writes a file; it is not fd duplication.
	{"echo x >&1out.txt", false, true, "R2 fd dup with a file name glued on"},
	{"echo x >&-out.txt", false, true, "R2 fd close with a file name glued on"},
	{"echo x >&2x", false, true, "R2 digit then letters"},
	{"echo hi >>&1", false, true, "append to fd: not fd dup"},
	// Intended relaxations: fd duplication and /dev/null stay read-only.
	{"echo x 2>&1", true, false, "fd dup is an intended relaxation"},
	{"echo x >&2", true, false, "fd dup to stderr"},
	{"echo x 1>&2", true, false, "fd dup with digit"},
	{"echo x >/dev/null", true, false, "redirect to /dev/null"},
	{"echo x &>/dev/null", true, false, "combined discard"},
	// R3: loop and printf variables that change what runs.
	{"for PATH in . ; do ls; done", false, true, "R3 loop over PATH"},
	{"for IFS in x; do ls; done", false, true, "R3 loop over IFS"},
	{"for f in a; do ls; done", true, false, "intended: complete one-line loop"},
	{"printf -v PATH . ; ls", false, false, "R3 printf -v: gated, not destructive"},
	{"printf -vPATH . ; ls", false, false, "R3 printf -v attached: gated"},
	{"printf x", true, false, "printf without -v is a read"},
	// Assignments that change execution.
	{"PATH=. ls", false, true, "PATH assignment"},
	{"LD_PRELOAD=x.so ls", false, true, "loader assignment"},
	// R5: wrappers and quoted command words hide the command. Gated.
	{"\\rm x", false, true, "R5 backslash command word"},
	{"r\"\"m x", false, true, "R5 quoted command word"},
	{"'r'm x", false, true, "R5 single-quoted command word"},
	{"env rm x", false, true, "R5 env wrapper"},
	{"command rm x", false, true, "R5 command wrapper"},
	{"nice rm x", false, true, "R5 nice wrapper"},
	{"time rm x", false, true, "R5 time wrapper"},
	{"nohup rm x", false, true, "R5 nohup wrapper"},
	{"timeout 1 rm x", false, true, "R5 timeout wrapper"},
	{"eval 'rm x'", false, true, "R5 eval"},
	{"exec rm x", false, true, "R5 exec"},
	{"git -C . reset --hard", false, true, "R5 git -C then reset"},
	{"git -C . status", true, false, "git -C is classified by its subcommand"},
	{"git -c core.pager=./p.sh log", false, true, "git -c gated"},
	{"git --git-dir=x status", false, true, "git global option gated"},
	// R4: git and flag forms that execute or write.
	{"git grep -O./p.sh x", false, false, "R4 git grep -O attached"},
	{"git grep --open-files=./p.sh x", false, false, "R4 git grep --open-files"},
	{"git grep --open-files-in-pager p", false, false, "R4 open-files-in-pager"},
	{"git ls-remote --upload-pack=./u.sh .", false, false, "R4 ls-remote upload-pack"},
	{"git config -- k --list", false, false, "R4 git config writes k"},
	{"git config user.name x", false, false, "git config write"},
	{"git config --get user.name", true, false, "git config read"},
	{"git config --list", true, false, "git config list"},
	{"git diff --ext-diff", false, false, "git ext-diff enables a program"},
	{"git diff --ext-diff=false", true, false, "ext-diff switched off"},
	{"git diff --no-textconv", true, false, "textconv switched off"},
	{"git diff --output-indicator-new=+", false, false, "output prefix gated"},
	{"fd -Hx rm", false, false, "R4 fd -x in a cluster"},
	{"fd -xrm", false, false, "R4 fd -x attached"},
	{"fd --exec=rm", false, false, "R4 fd --exec attached"},
	{"tree -ao out", false, false, "R4 tree -o in a cluster"},
	{"less -ao out f", false, false, "R4 less -o in a cluster"},
	{"yq -Pi .a f", false, false, "R4 yq -i in a cluster"},
	{"yq -ie .a f", false, false, "R4 yq -i first"},
	{"hostname -F f", false, false, "R4 hostname -F reads a file"},
	{"hostname --file=f", false, false, "R4 hostname --file"},
	{"xxd - out", false, false, "R4 xxd second positional"},
	{"xxd f", true, false, "xxd read"},
	{"date -s 2020", false, false, "R4 date -s sets the clock"},
	{"date -us 2020", false, false, "R4 date -s in a cluster"},
	{"date +%Y-%m-%d", true, false, "date format read"},
	// R6: pure reads that were over-gated stay gated (fail closed), except
	// the sed relaxations the task lists.
	{"sed -n p -- -i", false, true, "sed -- ends options: gated"},
	{"sed -n 'w out' f", false, true, "sed w writes"},
	{"sed -i s/a/b/ f", false, true, "sed -i"},
	{"sed -n p f", true, false, "sed print form is an intended relaxation"},
	// Substitutions: read-only only when both inner and outer are read-only.
	{"echo $(ls)", true, false, "substitution, read-only inner and outer"},
	{"git diff $(echo --output=x)", false, true, "substitution argument to git"},
	{"x=$(ls)", true, false, "assignment from a read-only substitution"},
	{"echo $(rm x)", false, true, "destructive substitution"},
	// Review round: forms that reached read-only through a too-broad exemption,
	// an unchecked reader, a variable expansion or a bracket that hid a "w".
	{"git diff --output=false", false, false, "=false is not a switch for --output"},
	{"git log --output=/tmp/x=false", false, false, "output file named with =false"},
	{"git reflog show --outp=/tmp/pwn", false, false, "reflog abbreviated --output"},
	{"git stash list --outp=/tmp/pwn", false, false, "stash list abbreviated --output"},
	{"git reflog show --ext-diff", false, false, "reflog enables ext-diff"},
	{"printf $(echo -v) PATH /tmp/evil; ls", false, false, "substitution expands to printf -v"},
	{"for f in -v; do printf $f PATH /tmp/evil; ls; done", false, false, "loop var expands to printf -v"},
	{"echo pwned | sed s/[/]*/p#x/w/tmp/pwn", false, true, "bracket hides a w command in sed"},
	{"HOME=/tmp/evil git status", false, true, "HOME changes git config"},
	{"RIPGREP_CONFIG_PATH=/tmp/evil rg foo", false, true, "rg config path"},
	{"ACKRC=/tmp/evil ack foo", false, true, "ack config path"},
	{"ack --ackrc=/tmp/evil foo", false, false, "ack config file option"},
	{"bat --pager=/tmp/pwn f", false, false, "bat pager option"},
	{"bat --generate-config-file", false, false, "bat writes a config file"},
	{"file -C f", false, false, "file -C compiles a magic file to disk"},
	{"less --LOG-FILE=/tmp/pwn f", false, false, "less upper-case log file option"},
	{"yq --in-place .a f", false, false, "yq long in-place option"},
	{"yq --inplace=true .a f", false, false, "yq inplace with a value"},
	{"git diff --ext-diff=false", true, false, "ext-diff explicitly off"},
	{"git diff --no-textconv", true, false, "textconv explicitly off"},
	{"sed -n p f", true, false, "sed print form still read-only"},
	{"file f", true, false, "file read still read-only"},
	{"yq '.a' f", true, false, "yq read still read-only"},
	{"printf x", true, false, "printf without a variable"},

	// G3 / R8: the leading-redirect test row the CHANGELOG describes.
	{"2>&1 cat f", true, false, "G3 leading fd dup with a read-only command"},
	{"2>/dev/null ls", true, false, "G3 leading /dev/null with a read-only command"},
	// Loops and other shell forms: gated when not one complete read-only loop.
	{"while read l; do echo $l; done < f", false, true, "input redirect loop"},
	{"(cd sub && make)", false, true, "subshell group"},
	{"if true; then ls; fi", false, true, "if construct"},
}

func TestShellClassifierTable(t *testing.T) {
	for _, tc := range classifyTable {
		ro := IsReadOnlyShell(tc.cmd)
		d := IsDestructiveShell(tc.cmd)
		if ro != tc.ro {
			t.Errorf("IsReadOnlyShell(%q) = %v, want %v (%s)", tc.cmd, ro, tc.ro, tc.why)
		}
		if d != tc.d {
			t.Errorf("IsDestructiveShell(%q) = %v, want %v (%s)", tc.cmd, d, tc.d, tc.why)
		}
		if ro && d {
			t.Errorf("%q is both read-only and destructive (%s)", tc.cmd, tc.why)
		}
	}
}
