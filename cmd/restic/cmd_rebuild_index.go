package main

import (
	"bytes"
	"io"
	"io/ioutil"

	"github.com/restic/restic/backend"
	"github.com/restic/restic/debug"
	"github.com/restic/restic/pack"
	"github.com/restic/restic/repository"
)

type CmdRebuildIndex struct {
	global *GlobalOptions

	repo *repository.Repository
}

func init() {
	_, err := parser.AddCommand("rebuild-index",
		"rebuild the index",
		"The rebuild-index command builds a new index",
		&CmdRebuildIndex{global: &globalOpts})
	if err != nil {
		panic(err)
	}
}

func (cmd CmdRebuildIndex) storeIndex(index *repository.Index) (*repository.Index, error) {
	debug.Log("RebuildIndex.RebuildIndex", "saving index")

	cmd.global.Printf("  saving new index\n")
	id, err := repository.SaveIndex(cmd.repo, index)
	if err != nil {
		debug.Log("RebuildIndex.RebuildIndex", "error saving index: %v", err)
		return nil, err
	}

	debug.Log("RebuildIndex.RebuildIndex", "index saved as %v", id.Str())
	index = repository.NewIndex()

	return index, nil
}

func (cmd CmdRebuildIndex) RebuildIndex() error {
	debug.Log("RebuildIndex.RebuildIndex", "start")

	done := make(chan struct{})
	defer close(done)

	indexIDs := backend.NewIDSet()
	for id := range cmd.repo.List(backend.Index, done) {
		indexIDs.Insert(id)
	}

	cmd.global.Printf("rebuilding index from %d indexes\n", len(indexIDs))

	debug.Log("RebuildIndex.RebuildIndex", "found %v indexes", len(indexIDs))

	combinedIndex := repository.NewIndex()
	packsDone := backend.NewIDSet()

	type Blob struct {
		id  backend.ID
		tpe pack.BlobType
	}
	blobsDone := make(map[Blob]struct{})

	i := 0
	for indexID := range indexIDs {
		cmd.global.Printf("  loading index %v\n", i)

		debug.Log("RebuildIndex.RebuildIndex", "load index %v", indexID.Str())
		idx, err := repository.LoadIndex(cmd.repo, indexID.String())
		if err != nil {
			return err
		}

		debug.Log("RebuildIndex.RebuildIndex", "adding blobs from index %v", indexID.Str())

		for packedBlob := range idx.Each(done) {
			packsDone.Insert(packedBlob.PackID)
			b := Blob{
				id:  packedBlob.ID,
				tpe: packedBlob.Type,
			}
			if _, ok := blobsDone[b]; ok {
				continue
			}

			blobsDone[b] = struct{}{}
			combinedIndex.Store(packedBlob)
		}

		combinedIndex.AddToSupersedes(indexID)

		if repository.IndexFull(combinedIndex) {
			combinedIndex, err = cmd.storeIndex(combinedIndex)
			if err != nil {
				return err
			}
		}

		i++
	}

	var err error
	if combinedIndex.Length() > 0 {
		combinedIndex, err = cmd.storeIndex(combinedIndex)
		if err != nil {
			return err
		}
	}

	cmd.global.Printf("removing %d old indexes\n", len(indexIDs))
	for id := range indexIDs {
		debug.Log("RebuildIndex.RebuildIndex", "remove index %v", id.Str())

		err := cmd.repo.Backend().Remove(backend.Index, id.String())
		if err != nil {
			debug.Log("RebuildIndex.RebuildIndex", "error removing index %v: %v", id.Str(), err)
			return err
		}
	}

	cmd.global.Printf("checking for additional packs\n")
	newPacks := 0
	for packID := range cmd.repo.List(backend.Data, done) {
		if packsDone.Has(packID) {
			continue
		}

		debug.Log("RebuildIndex.RebuildIndex", "pack %v not indexed", packID.Str())
		newPacks++

		rd, err := cmd.repo.Backend().GetReader(backend.Data, packID.String(), 0, 0)
		if err != nil {
			debug.Log("RebuildIndex.RebuildIndex", "GetReader returned error: %v", err)
			return err
		}

		var readSeeker io.ReadSeeker
		if r, ok := rd.(io.ReadSeeker); ok {
			debug.Log("RebuildIndex.RebuildIndex", "reader is seekable")
			readSeeker = r
		} else {
			debug.Log("RebuildIndex.RebuildIndex", "reader is not seekable, loading contents to ram")
			buf, err := ioutil.ReadAll(rd)
			if err != nil {
				return err
			}

			readSeeker = bytes.NewReader(buf)
		}

		up, err := pack.NewUnpacker(cmd.repo.Key(), readSeeker)
		if err != nil {
			debug.Log("RebuildIndex.RebuildIndex", "error while unpacking pack %v", packID.Str())
			return err
		}

		for _, blob := range up.Entries {
			debug.Log("RebuildIndex.RebuildIndex", "pack %v: blob %v", packID.Str(), blob)
			combinedIndex.Store(repository.PackedBlob{
				Type:   blob.Type,
				ID:     blob.ID,
				PackID: packID,
				Offset: blob.Offset,
				Length: blob.Length,
			})
		}

		err = rd.Close()
		debug.Log("RebuildIndex.RebuildIndex", "error closing reader for pack %v: %v", packID.Str(), err)

		if repository.IndexFull(combinedIndex) {
			combinedIndex, err = cmd.storeIndex(combinedIndex)
			if err != nil {
				return err
			}
		}
	}

	if combinedIndex.Length() > 0 {
		combinedIndex, err = cmd.storeIndex(combinedIndex)
		if err != nil {
			return err
		}
	}

	cmd.global.Printf("added %d packs to the index\n", newPacks)

	debug.Log("RebuildIndex.RebuildIndex", "done")
	return nil
}

func (cmd CmdRebuildIndex) Execute(args []string) error {
	repo, err := cmd.global.OpenRepository()
	if err != nil {
		return err
	}
	cmd.repo = repo

	lock, err := lockRepoExclusive(repo)
	defer unlockRepo(lock)
	if err != nil {
		return err
	}

	return cmd.RebuildIndex()
}
