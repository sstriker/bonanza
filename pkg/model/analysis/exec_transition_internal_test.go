package analysis

import (
	"testing"

	"bonanza.build/pkg/label"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"

	"google.golang.org/protobuf/types/known/emptypb"
)

func TestExecStarlarkFlagPropagation(t *testing.T) {
	flag := util.Must(label.NewCanonicalLabel("@@my_module+//foo/bar:flag"))
	defaultScope := (*model_starlark_pb.BuildSettingScope)(nil)
	targetScope := &model_starlark_pb.BuildSettingScope{Kind: &model_starlark_pb.BuildSettingScope_Target{Target: &emptypb.Empty{}}}
	projectScope := &model_starlark_pb.BuildSettingScope{Kind: &model_starlark_pb.BuildSettingScope_Project{Project: &emptypb.Empty{}}}
	universalScope := &model_starlark_pb.BuildSettingScope{Kind: &model_starlark_pb.BuildSettingScope_Universal{Universal: &emptypb.Empty{}}}

	t.Run("PropagateByDefault", func(t *testing.T) {
		// Bazel 9's default: build settings propagate, unless
		// they are explicitly target scoped.
		p := execStarlarkFlagPropagation{}
		require.True(t, p.shouldPropagate(flag, defaultScope))
		require.False(t, p.shouldPropagate(flag, targetScope))
		require.True(t, p.shouldPropagate(flag, projectScope))
		require.True(t, p.shouldPropagate(flag, universalScope))
	})

	t.Run("ExcludeByDefault", func(t *testing.T) {
		// With --incompatible_exclude_starlark_flags_from_exec_config,
		// only universal and project scoped build settings
		// propagate.
		p := execStarlarkFlagPropagation{excludeByDefault: true}
		require.False(t, p.shouldPropagate(flag, defaultScope))
		require.False(t, p.shouldPropagate(flag, targetScope))
		require.True(t, p.shouldPropagate(flag, projectScope))
		require.True(t, p.shouldPropagate(flag, universalScope))
	})

	t.Run("CustomFlags", func(t *testing.T) {
		// --experimental_propagate_custom_flag can be used to
		// propagate individual build settings, or all build
		// settings in a package and its subpackages. Build
		// settings that are explicitly target scoped never
		// propagate.
		for _, tc := range []struct {
			name      string
			p         execStarlarkFlagPropagation
			propagate bool
		}{
			{"Label", execStarlarkFlagPropagation{customFlags: map[string]struct{}{"@@my_module+//foo/bar:flag": {}}}, true},
			{"OtherLabel", execStarlarkFlagPropagation{customFlags: map[string]struct{}{"@@my_module+//foo/bar:other": {}}}, false},
			{"SamePackage", execStarlarkFlagPropagation{customFlagPackages: []label.CanonicalPackage{util.Must(label.NewCanonicalPackage("@@my_module+//foo/bar"))}}, true},
			{"ParentPackage", execStarlarkFlagPropagation{customFlagPackages: []label.CanonicalPackage{util.Must(label.NewCanonicalPackage("@@my_module+//foo"))}}, true},
			{"RootPackage", execStarlarkFlagPropagation{customFlagPackages: []label.CanonicalPackage{util.Must(label.NewCanonicalPackage("@@my_module+"))}}, true},
			{"SiblingPackage", execStarlarkFlagPropagation{customFlagPackages: []label.CanonicalPackage{util.Must(label.NewCanonicalPackage("@@my_module+//fo"))}}, false},
			{"OtherRepo", execStarlarkFlagPropagation{customFlagPackages: []label.CanonicalPackage{util.Must(label.NewCanonicalPackage("@@other_module+//foo"))}}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.p.excludeByDefault = true
				require.Equal(t, tc.propagate, tc.p.shouldPropagate(flag, defaultScope))
				require.False(t, tc.p.shouldPropagate(flag, targetScope))
			})
		}
	})
}
