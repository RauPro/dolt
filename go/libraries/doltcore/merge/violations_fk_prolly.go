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

package merge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/durable"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor/creation"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/shim"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
)

func prollyParentFkConstraintViolations(
	ctx context.Context,
	foreignKey doltdb.ForeignKey,
	postParent, postChild *constraintViolationsLoadedTable,
	preParentRowData prolly.Map,
	theirRootIsh hash.Hash,
	jsonData []byte) (*doltdb.Table, bool, error) {
	postParentRowData := durable.ProllyMapFromIndex(postParent.RowData)
	postParentIndexData := durable.ProllyMapFromIndex(postParent.IndexData)

	idxDesc := shim.KeyDescriptorFromSchema(postParent.Index.Schema())
	partialDesc := idxDesc.PrefixDesc(len(foreignKey.TableColumns))
	partialKB := val.NewTupleBuilder(partialDesc)

	artIdx, err := postChild.Table.GetArtifacts(ctx)
	if err != nil {
		return nil, false, err
	}
	artM := durable.ProllyMapFromArtifactIndex(artIdx)
	artEditor := artM.Editor()

	childPriIdx := durable.ProllyMapFromIndex(postChild.RowData)
	childScndryIdx := durable.ProllyMapFromIndex(postChild.IndexData)
	primaryKD, _ := childPriIdx.Descriptors()

	var foundViolation bool

	err = prolly.DiffMaps(ctx, preParentRowData, postParentRowData, func(ctx context.Context, diff tree.Diff) error {
		switch diff.Type {
		case tree.RemovedDiff, tree.ModifiedDiff:
			partialKey, hadNulls := makePartialKey(partialKB, postParent.Index, postParent.Schema, val.Tuple(diff.Key), val.Tuple(diff.From), preParentRowData.Pool())
			if hadNulls {
				// row had some nulls previously, so it couldn't have been a parent
				return nil
			}

			partialKeyRange := prolly.PrefixRange(partialKey, partialDesc)
			itr, err := postParentIndexData.IterRange(ctx, partialKeyRange)
			if err != nil {
				return err
			}

			_, _, err = itr.Next(ctx)
			if err != nil && err != io.EOF {
				return err
			}
			if err == nil {
				// some other equivalent parents exist
				return nil
			}

			// All equivalent parents were deleted, let's check for dangling children.
			// We search for matching keys in the child's secondary index
			found, err := createCVsForPartialKeyMatches(ctx, partialKey, partialDesc, artEditor, primaryKD, childPriIdx, childScndryIdx, childPriIdx.Pool(), jsonData, theirRootIsh, postChild.TableName)
			if err != nil {
				return err
			}

			foundViolation = foundViolation || found

		case tree.AddedDiff:
		default:
			panic("unhandled diff type")
		}

		return nil
	})
	if err != nil && err != io.EOF {
		return nil, false, err
	}

	artM, err = artEditor.Flush(ctx)
	if err != nil {
		return nil, false, err
	}

	updated, err := postChild.Table.SetArtifacts(ctx, durable.ArtifactIndexFromProllyMap(artM))
	if err != nil {
		return nil, false, err
	}

	return updated, foundViolation, nil
}

func prollyChildFkConstraintViolations(
	ctx context.Context,
	foreignKey doltdb.ForeignKey,
	postParent, postChild *constraintViolationsLoadedTable,
	preChildRowData prolly.Map,
	theirRootIsh hash.Hash,
	jsonData []byte) (*doltdb.Table, bool, error) {
	postChildRowData := durable.ProllyMapFromIndex(postChild.RowData)

	idxDesc := shim.KeyDescriptorFromSchema(postChild.Index.Schema())
	partialDesc := idxDesc.PrefixDesc(len(foreignKey.TableColumns))
	partialKB := val.NewTupleBuilder(partialDesc)

	artIdx, err := postChild.Table.GetArtifacts(ctx)
	if err != nil {
		return nil, false, err
	}
	artM := durable.ProllyMapFromArtifactIndex(artIdx)
	artEditor := artM.Editor()

	parentScndryIdx := durable.ProllyMapFromIndex(postParent.IndexData)

	var foundViolation bool
	kd, vd := postChildRowData.Descriptors()

	err = prolly.DiffMaps(ctx, preChildRowData, postChildRowData, func(ctx context.Context, diff tree.Diff) error {
		switch diff.Type {
		case tree.AddedDiff, tree.ModifiedDiff:
			k, v := val.Tuple(diff.Key), val.Tuple(diff.To)
			partialKey, hasNulls := makePartialKey(partialKB, postChild.Index, postChild.Schema, k, v, preChildRowData.Pool())
			if hasNulls {
				return nil
			}

			found, err := createCVIfNoPartialKeyMatches(ctx, k, v, partialKey, kd, vd, partialDesc, parentScndryIdx, artEditor, jsonData, theirRootIsh, postChild.TableName)
			if err != nil {
				return err
			}
			foundViolation = foundViolation || found
		case tree.RemovedDiff:
		default:
			panic("unhandled diff type")
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, false, err
	}

	artM, err = artEditor.Flush(ctx)
	if err != nil {
		return nil, false, err
	}

	updated, err := postChild.Table.SetArtifacts(ctx, durable.ArtifactIndexFromProllyMap(artM))
	if err != nil {
		return nil, false, err
	}

	return updated, foundViolation, nil
}

func createCVIfNoPartialKeyMatches(
	ctx context.Context,
	k, v, partialKey val.Tuple,
	kd, vd, partialKeyDesc val.TupleDesc,
	idx prolly.Map,
	editor prolly.ArtifactsEditor,
	jsonData []byte,
	theirRootIsh hash.Hash,
	tblName string) (bool, error) {
	itr, err := creation.NewPrefixItr(ctx, partialKey, partialKeyDesc, idx)
	if err != nil {
		return false, err
	}
	_, _, err = itr.Next(ctx)
	if err != nil && err != io.EOF {
		return false, err
	}
	if err == nil {
		return false, nil
	}

	meta := prolly.ConstraintViolationMeta{VInfo: jsonData, Value: v}

	err = editor.ReplaceConstraintViolation(ctx, k, theirRootIsh, prolly.ArtifactTypeForeignKeyViol, meta)
	if err != nil {
		return false, handleFkMultipleViolForRowErr(err, kd, tblName)
	}

	return true, nil
}

func handleFkMultipleViolForRowErr(err error, kd val.TupleDesc, tblName string) error {
	if mv, ok := err.(*prolly.ErrMergeArtifactCollision); ok {
		var e, n FkCVMeta
		err = json.Unmarshal(mv.ExistingInfo, &e)
		if err != nil {
			return err
		}
		err = json.Unmarshal(mv.NewInfo, &n)
		if err != nil {
			return err
		}
		return fmt.Errorf(`%w: pk %s of table '%s' violates foreign keys '%s' and '%s'`,
			ErrMultipleViolationsForRow,
			kd.Format(mv.Key), tblName, getRefTblAndCols(e), getRefTblAndCols(n))
	}
	return err
}

func getRefTblAndCols(m FkCVMeta) string {
	return fmt.Sprintf("%s (%s)", m.ReferencedTable, strings.Join(m.ReferencedColumns, ", "))
}

func createCVsForPartialKeyMatches(
	ctx context.Context,
	partialKey val.Tuple,
	partialKeyDesc val.TupleDesc,
	editor prolly.ArtifactsEditor,
	primaryKD val.TupleDesc,
	primaryIdx prolly.Map,
	secondaryIdx prolly.Map,
	pool pool.BuffPool,
	jsonData []byte,
	theirRootIsh hash.Hash,
	tblName string,
) (bool, error) {
	createdViolation := false

	itr, err := creation.NewPrefixItr(ctx, partialKey, partialKeyDesc, secondaryIdx)
	if err != nil {
		return false, err
	}

	kb := val.NewTupleBuilder(primaryKD)

	for k, _, err := itr.Next(ctx); err == nil; k, _, err = itr.Next(ctx) {
		createdViolation = true

		// convert secondary idx entry to primary row key
		// the pks of the table are the last keys of the index
		o := k.Count() - primaryKD.Count()
		for i := 0; i < primaryKD.Count(); i++ {
			j := o + i
			kb.PutRaw(i, k.GetField(j))
		}
		primaryIdxKey := kb.Build(pool)

		var value val.Tuple
		err := primaryIdx.Get(ctx, primaryIdxKey, func(k, v val.Tuple) error {
			value = v
			return nil
		})
		if err != nil {
			return false, err
		}
		meta := prolly.ConstraintViolationMeta{VInfo: jsonData, Value: value}

		err = editor.ReplaceConstraintViolation(ctx, primaryIdxKey, theirRootIsh, prolly.ArtifactTypeForeignKeyViol, meta)
		if err != nil {
			return false, handleFkMultipleViolForRowErr(err, primaryKD, tblName)
		}
	}
	if err != nil && err != io.EOF {
		return false, err
	}

	return createdViolation, nil
}

func makePartialDescriptor(desc val.TupleDesc, n int) val.TupleDesc {
	return val.NewTupleDescriptor(desc.Types[:n]...)
}

func makePartialKey(kb *val.TupleBuilder, idxSch schema.Index, tblSch schema.Schema, k, v val.Tuple, pool pool.BuffPool) (val.Tuple, bool) {
	for i, tag := range idxSch.IndexedColumnTags() {
		if j, ok := tblSch.GetPKCols().TagToIdx[tag]; ok {
			if k.FieldIsNull(j) {
				return nil, true
			}
			kb.PutRaw(i, k.GetField(j))
			continue
		}

		j, _ := tblSch.GetNonPKCols().TagToIdx[tag]
		if v.FieldIsNull(j) {
			return nil, true
		}
		if schema.IsKeyless(tblSch) {
			kb.PutRaw(i, v.GetField(j+1))
		} else {
			kb.PutRaw(i, v.GetField(j))
		}
	}

	return kb.Build(pool), false
}

// TODO: Change json.NomsJson string marshalling to match json.Marshall
// Currently it returns additional whitespace. Another option is to implement a
// custom json encoder that matches json.NomsJson string marshalling.

type FkCVMeta struct {
	Columns           []string `json:"Columns"`
	ForeignKey        string   `json:"ForeignKey"`
	Index             string   `json:"Index"`
	OnDelete          string   `json:"OnDelete"`
	OnUpdate          string   `json:"OnUpdate"`
	ReferencedColumns []string `json:"ReferencedColumns"`
	ReferencedIndex   string   `json:"ReferencedIndex"`
	ReferencedTable   string   `json:"ReferencedTable"`
	Table             string   `json:"Table"`
}

func (m FkCVMeta) Unmarshall(ctx *sql.Context) (val sql.JSONDocument, err error) {
	return sql.JSONDocument{Val: m}, nil
}

func (m FkCVMeta) Compare(ctx *sql.Context, v sql.JSONValue) (cmp int, err error) {
	ours := sql.JSONDocument{Val: m}
	return ours.Compare(ctx, v)
}

func (m FkCVMeta) ToString(ctx *sql.Context) (string, error) {
	return m.PrettyPrint(), nil
}

var _ sql.JSONValue = FkCVMeta{}

// PrettyPrint is a custom pretty print function to match the old format's
// output which includes additional whitespace between keys, values, and array elements.
func (m FkCVMeta) PrettyPrint() string {
	jsonStr := fmt.Sprintf(`{`+
		`"Columns": ["%s"], `+
		`"ForeignKey": "%s", `+
		`"Index": "%s", `+
		`"OnDelete": "%s", `+
		`"OnUpdate": "%s", `+
		`"ReferencedColumns": ["%s"], `+
		`"ReferencedIndex": "%s", `+
		`"ReferencedTable": "%s", `+
		`"Table": "%s"}`,
		strings.Join(m.Columns, `', '`),
		m.ForeignKey,
		m.Index,
		m.OnDelete,
		m.OnUpdate,
		strings.Join(m.ReferencedColumns, `', '`),
		m.ReferencedIndex,
		m.ReferencedTable,
		m.Table)
	return jsonStr
}
