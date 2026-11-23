package starlark_test

import (
	"testing"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	model_starlark "bonanza.build/pkg/model/starlark"
	"bonanza.build/pkg/starlark/eval"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"
)

func TestLabelStrAndRepr(t *testing.T) {
	// As in Bazel, str() of a label yields the label itself, while
	// repr() yields an expression that constructs it. Labels
	// contained in other values are formatted using repr().
	_, err := execBzlFile(
		newTestThread(t),
		util.Must(label.NewCanonicalLabel("@@main+//:test.bzl")),
		`
def check(got, want):
    if got != want:
        fail("got %r, want %r" % (got, want))

def test():
    check(str(l), "@@rules+//foo:bar")
    check(repr(l), 'Label("@@rules+//foo:bar")')
    check("%s" % l, "@@rules+//foo:bar")
    check("%r" % l, 'Label("@@rules+//foo:bar")')
    check("{}".format(l), "@@rules+//foo:bar")
    check(str([l]), '[Label("@@rules+//foo:bar")]')

test()
`,
		map[string]eval.Value{
			"l": model_starlark.NewLabel[object.LocalReference, model_core.CreatedObjectTree](util.Must(label.NewResolvedLabel("@@rules+//foo:bar"))),
		},
	)
	require.NoError(t, err)
}

func TestLabelAttributes(t *testing.T) {
	_, err := execBzlFile(
		newTestThread(t),
		util.Must(label.NewCanonicalLabel("@@main+//:test.bzl")),
		`
def check(got, want):
    if got != want:
        fail("got %r, want %r" % (got, want))

def test():
    check(l.name, "bar")
    check(l.package, "foo")
    check(l.repo_name, "rules+")
    check(l.workspace_name, "rules+")
    check(l.workspace_root, "external/rules+")
    check(str(l.same_package_label("baz")), "@@rules+//foo:baz")
    check(dir(l), ["name", "package", "relative", "repo_name", "same_package_label", "workspace_name", "workspace_root"])

test()
`,
		map[string]eval.Value{
			"l": model_starlark.NewLabel[object.LocalReference, model_core.CreatedObjectTree](util.Must(label.NewResolvedLabel("@@rules+//foo:bar"))),
		},
	)
	require.NoError(t, err)
}
