package starlark

import (
	"fmt"

	pg_label "bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"go.starlark.net/starlark"
)

// RepoRegistrar is used to track invocations of repository rules during
// the evaluation of module extensions.
type RepoRegistrar[TMetadata model_core.ReferenceMetadata] struct {
	repos map[string]model_core.PatchedMessage[*model_starlark_pb.Repo, TMetadata]
}

// NewRepoRegistrar creates a new RepoRegistrar that is in the initial
// state (i.e., having no registered repositories).
func NewRepoRegistrar[TMetadata model_core.ReferenceMetadata]() *RepoRegistrar[TMetadata] {
	return &RepoRegistrar[TMetadata]{
		repos: map[string]model_core.PatchedMessage[*model_starlark_pb.Repo, TMetadata]{},
	}
}

// GetRepos returns the set of repositories that have been registered by a
// previously evaluated module extension.
func (rr *RepoRegistrar[TMetadata]) GetRepos() map[string]model_core.PatchedMessage[*model_starlark_pb.Repo, TMetadata] {
	return rr.repos
}

func (rr *RepoRegistrar[TMetadata]) registerRepo(name string, repo model_core.PatchedMessage[*model_starlark_pb.Repo, TMetadata]) error {
	if _, ok := rr.repos[name]; ok {
		return fmt.Errorf("module extension contains multiple repos with name %#v", name)
	}
	rr.repos[name] = repo
	return nil
}

// GetExistingRepo returns information on a previously declared repo
// with a given name in the shape used by native.existing_rule(), or
// nil if no repo with that name has been declared by the module
// extension so far.
func (rr *RepoRegistrar[TMetadata]) GetExistingRepo(thread *starlark.Thread, name string) map[string]starlark.Value {
	repo, ok := rr.repos[name]
	if !ok {
		return nil
	}
	existingRepo := map[string]starlark.Value{
		"name": starlark.String(name),
	}
	if identifierStr := repo.Message.GetDefinition().GetRepositoryRuleIdentifier(); identifierStr != "" {
		if identifier, err := pg_label.NewCanonicalStarlarkIdentifier(identifierStr); err == nil {
			existingRepo["kind"] = starlark.String(identifier.GetStarlarkIdentifier().String())
		}
	}
	return existingRepo
}

// GetExistingRepos returns information on all repos that have been
// declared by the module extension so far, in the shape used by
// native.existing_rules().
func (rr *RepoRegistrar[TMetadata]) GetExistingRepos(thread *starlark.Thread) map[string]map[string]starlark.Value {
	existingRepos := make(map[string]map[string]starlark.Value, len(rr.repos))
	for name := range rr.repos {
		existingRepos[name] = rr.GetExistingRepo(thread, name)
	}
	return existingRepos
}
