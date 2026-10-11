package git

import (
	"reflect"
	"testing"
)

func TestInertConfigKey(t *testing.T) {
	inert := []string{
		"branch.main.remote", "branch.main.merge", "branch.main.rebase",
		"branch.main.pushRemote", "branch.main.description",
		"branch.spec/wps-bridge-t2.vscode-merge-base", // TASK-000094's key
		"branch.release.v1.2.gk-merge-target",         // dots in the branch name
		"branch.feature.github-pr-owner-number",
		"BRANCH.Main.VSCODE-MERGE-BASE",
	}
	for _, key := range inert {
		if !InertConfigKey(key) {
			t.Errorf("InertConfigKey(%q) = false, want true", key)
		}
	}
	dangerous := []string{
		"core.hookspath", "core.fsmonitor", "core.sshcommand", "core.pager",
		"include.path", "includeif.gitdir:/x.path",
		"filter.lfs.smudge", "diff.x.textconv", "merge.x.driver",
		"alias.st", "credential.helper", "url.https://evil/.insteadof",
		"remote.origin.url", "remote.origin.uploadpack",
		"branch.main.mergeoptions", // chooses a merge strategy, an executable
		"branch.main",              // malformed: no variable
		"branch..remote",
		"aidev.probe", // unknown keys run nothing today, but are not known to
	}
	for _, key := range dangerous {
		if InertConfigKey(key) {
			t.Errorf("InertConfigKey(%q) = true, want false", key)
		}
	}
}

func TestParseConfigListAndChangedKeys(t *testing.T) {
	before := parseConfigList("core.bare\nfalse\x00remote.origin.fetch\n+a\x00remote.origin.fetch\n+b\x00core.hookspath\x00")
	want := map[string][]string{
		"core.bare":           {"false"},
		"remote.origin.fetch": {"+a", "+b"},
		"core.hookspath":      {""},
	}
	if !reflect.DeepEqual(before, want) {
		t.Fatalf("parseConfigList = %v, want %v", before, want)
	}
	after := parseConfigList("core.bare\nfalse\x00remote.origin.fetch\n+a\x00Branch.X.Remote\norigin\x00")
	got := changedConfigKeys(before, after)
	wantKeys := []string{"branch.x.remote", "core.hookspath", "remote.origin.fetch"}
	if !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("changedConfigKeys = %v, want %v", got, wantKeys)
	}
}
