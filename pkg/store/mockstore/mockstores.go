// GENERATED CODE - DO NOT EDIT!
//
// Generated by:
//
//   go run gen_context_and_mock.go -o1 context.go -o2 mockstore/mockstores.go
//
// Called via:
//
//   go generate
//

package mockstore

import (
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/store"
	srcstore "sourcegraph.com/sourcegraph/srclib/store"
)

// Stores has a field for each store interface with the concrete mock type (to obviate the need for tedious type assertions in test code).
type Stores struct {
	Accounts           Accounts
	BuildLogs          BuildLogs
	Builds             Builds
	Channel            Channel
	Directory          Directory
	ExternalAuthTokens ExternalAuthTokens
	GlobalDefs         GlobalDefs
	GlobalRefs         GlobalRefs
	Graph              srcstore.MockMultiRepoStore
	Orgs               Orgs
	Password           Password
	RepoConfigs        RepoConfigs
	RepoPerms          RepoPerms
	RepoStatuses       RepoStatuses
	RepoVCS            RepoVCS
	Repos              Repos
	Users              Users
}

func (s *Stores) Stores() store.Stores {
	return store.Stores{
		Accounts:           &s.Accounts,
		BuildLogs:          &s.BuildLogs,
		Builds:             &s.Builds,
		Channel:            &s.Channel,
		Directory:          &s.Directory,
		ExternalAuthTokens: &s.ExternalAuthTokens,
		GlobalDefs:         &s.GlobalDefs,
		GlobalRefs:         &s.GlobalRefs,
		Graph:              &s.Graph,
		Orgs:               &s.Orgs,
		Password:           &s.Password,
		RepoConfigs:        &s.RepoConfigs,
		RepoPerms:          &s.RepoPerms,
		RepoStatuses:       &s.RepoStatuses,
		RepoVCS:            &s.RepoVCS,
		Repos:              &s.Repos,
		Users:              &s.Users,
	}
}
