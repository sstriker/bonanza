package analysis_test

import (
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/stretchr/testify/require"

	"go.uber.org/mock/gomock"
)

func TestUsedModuleExtension(t *testing.T) {
	ctrl, ctx := gomock.WithContext(t.Context(), t)
	bct := newBaseComputerTester(ctrl)

	e := NewMockUsedModuleExtensionEnvironmentForTesting(ctrl)
	e.EXPECT().GetUsedModuleExtensionsValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.UsedModuleExtensions_Value{
		ModuleExtensions: []*model_analysis_pb.ModuleExtension{
			{Identifier: "@@rules_go+//go:extensions.bzl%go_sdk"},
			{Identifier: "@@rules_rust+//cargo/private:internal_extensions.bzl%i"},
			{Identifier: "@@rules_rust+//rust/private:internal_extensions.bzl%i"},
			{Identifier: "@@rules_rust+//rust:extensions.bzl%rust"},
		},
	})).AnyTimes()

	getIdentifiers := func(moduleExtension string) ([]string, error) {
		value, err := bct.computer.ComputeUsedModuleExtensionValue(ctx, &model_analysis_pb.UsedModuleExtension_Key{
			ModuleExtension: moduleExtension,
		}, e)
		if err != nil {
			return nil, err
		}
		var identifiers []string
		for _, moduleExtension := range value.Message.ModuleExtensions {
			identifiers = append(identifiers, moduleExtension.Identifier)
		}
		return identifiers, nil
	}

	t.Run("SingleModuleExtension", func(t *testing.T) {
		identifiers, err := getIdentifiers("rules_go++go_sdk")
		require.NoError(t, err)
		require.Equal(t, []string{"@@rules_go+//go:extensions.bzl%go_sdk"}, identifiers)

		identifiers, err = getIdentifiers("rules_rust++rust")
		require.NoError(t, err)
		require.Equal(t, []string{"@@rules_rust+//rust:extensions.bzl%rust"}, identifiers)
	})

	t.Run("SameName", func(t *testing.T) {
		// rules_rust declares two module extensions named "i"
		// in separate .bzl files. Both should be returned, so
		// that they can be evaluated together.
		identifiers, err := getIdentifiers("rules_rust++i")
		require.NoError(t, err)
		require.Equal(t, []string{
			"@@rules_rust+//cargo/private:internal_extensions.bzl%i",
			"@@rules_rust+//rust/private:internal_extensions.bzl%i",
		}, identifiers)
	})

	t.Run("NotFound", func(t *testing.T) {
		_, err := getIdentifiers("rules_rust++missing")
		require.EqualError(t, err, "module extension not found")
	})
}
