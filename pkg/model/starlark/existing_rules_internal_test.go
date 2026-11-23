package starlark

import (
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/starlark/eval"
	"bonanza.build/pkg/storage/object"

	"github.com/stretchr/testify/require"
)

func TestExistingRulesInModuleExtension(t *testing.T) {
	// Within module extensions, native.existing_rule() and
	// native.existing_rules() return the repos that the module
	// extension declared so far.
	repoRegistrar := NewRepoRegistrar[model_core.CreatedObjectTree]()
	require.NoError(t, repoRegistrar.registerRepo("zlib", model_core.NewSimplePatchedMessage[model_core.CreatedObjectTree](&model_starlark_pb.Repo{
		Name: "zlib",
		Definition: &model_starlark_pb.Repo_Definition{
			RepositoryRuleIdentifier: "@@bazel_tools+//tools/build_defs/repo:http.bzl%http_archive",
		},
	})))
	thread := eval.NewThread(t.Context(), eval.Universe())
	thread.SetLocal(RepoRegistrarKey, repoRegistrar)

	existingRule := eval.NewBuiltin("existing_rule", nativeExistingRule[object.LocalReference, model_core.CreatedObjectTree])
	existingRules := eval.NewBuiltin("existing_rules", nativeExistingRules[object.LocalReference, model_core.CreatedObjectTree])

	t.Run("ExistingRule", func(t *testing.T) {
		v, err := eval.Call(thread, existingRule, eval.Args{Positional: []eval.Value{eval.String("zlib")}})
		require.NoError(t, err)
		repr, err := eval.Repr(thread, v)
		require.NoError(t, err)
		require.Equal(t, `{"kind": "http_archive", "name": "zlib"}`, repr)

		v, err = eval.Call(thread, existingRule, eval.Args{Positional: []eval.Value{eval.String("nonexistent")}})
		require.NoError(t, err)
		require.Equal(t, eval.None, v)
	})

	t.Run("ExistingRules", func(t *testing.T) {
		v, err := eval.Call(thread, existingRules, eval.Args{})
		require.NoError(t, err)
		repr, err := eval.Repr(thread, v)
		require.NoError(t, err)
		require.Equal(t, `{"zlib": {"kind": "http_archive", "name": "zlib"}}`, repr)
	})
}
