// Copyright 2022 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dprocedures

import (
	"fmt"
	"strings"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/argparser"
	"github.com/dolthub/dolt/go/libraries/utils/config"

	"github.com/dolthub/go-mysql-server/sql"
)

// doltRemote is the stored procedure version of the CLI `dolt remote` command
func doltRemote(ctx *sql.Context, args ...string) (sql.RowIter, error) {
	res, err := doDoltRemote(ctx, args)
	if err != nil {
		return nil, err
	}
	return rowToIter(res), nil
}

// doDoltRemote is used as sql dolt_remote command for only creating or deleting remotes, not listing.
// To list remotes, dolt_remotes system table is used.
func doDoltRemote(ctx *sql.Context, args []string) (int, error) {
	dbName := ctx.GetCurrentDatabase()
	if len(dbName) == 0 {
		return 1, fmt.Errorf("Empty database name.")
	}
	dSess := dsess.DSessFromSess(ctx.Session)
	dbData, ok := dSess.GetDbData(ctx, dbName)
	if !ok {
		return 1, fmt.Errorf("Could not load database %s", dbName)
	}

	apr, err := cli.CreateRemoteArgParser().Parse(args)
	if err != nil {
		return 1, err
	}

	if apr.NArg() == 0 {
		return 1, fmt.Errorf("error: invalid argument, use 'dolt_remotes' system table to list remotes")
	}

	switch apr.Arg(0) {
	case "add":
		err = addRemote(apr, dSess)
	case "remove", "rm":
		err = removeRemote(ctx, dbData, apr, dSess)
	default:
		err = fmt.Errorf("error: invalid argument")
	}

	if err != nil {
		return 1, err
	}
	return 0, nil
}

func addRemote(apr *argparser.ArgParseResults, sess *dsess.DoltSession) error {
	if apr.NArg() != 3 {
		return fmt.Errorf("error: invalid argument")
	}

	remoteName := strings.TrimSpace(apr.Arg(1))
	remoteUrl := apr.Arg(2)

	scheme, absRemoteUrl, err := env.GetAbsRemoteUrl(sess.Provider().FileSystem(), &config.MapConfig{}, remoteUrl)
	if err != nil {
		return err
	}

	params, err := parseRemoteArgs(apr, scheme, absRemoteUrl)
	if err != nil {
		return err
	}

	fs := sess.Provider().FileSystem()
	r := env.NewRemote(remoteName, absRemoteUrl, params, nil)

	repoState, err := env.LoadRepoState(fs)
	if err != nil {
		return err
	}
	repoState.AddRemote(r)
	err = repoState.Save(fs)
	if err != nil {
		return err
	}
	return nil
}

func removeRemote(ctx *sql.Context, dbd env.DbData, apr *argparser.ArgParseResults, sess *dsess.DoltSession) error {
	if apr.NArg() != 2 {
		return fmt.Errorf("error: invalid argument")
	}

	old := strings.TrimSpace(apr.Arg(1))

	fs := sess.Provider().FileSystem()
	repoState, err := env.LoadRepoState(fs)
	if err != nil {
		return err
	}

	remote, ok := repoState.Remotes[old]
	if !ok {
		return fmt.Errorf("error: unknown remote: '%s'", old)
	}

	ddb := dbd.Ddb
	refs, err := ddb.GetRemoteRefs(ctx)
	if err != nil {
		return fmt.Errorf("error: failed to read from db, cause: %s", env.ErrFailedToReadFromDb.Error())
	}

	for _, r := range refs {
		rr := r.(ref.RemoteRef)

		if rr.GetRemote() == remote.Name {
			err = ddb.DeleteBranch(ctx, rr)

			if err != nil {
				return fmt.Errorf("%w; failed to delete remote tracking ref '%s'; %s", env.ErrFailedToDeleteRemote, rr.String(), err.Error())
			}
		}
	}

	delete(repoState.Remotes, remote.Name)
	err = repoState.Save(fs)
	if err != nil {
		return err
	}

	return nil
}