package persistence

import (
	"context"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/bundles/types"
)

// KeyedDocumentData pairs a document with its path.
type KeyedDocumentData struct {
	Path     string
	Document types.DocumentData
}

// IndexedResultChunkData pairs a result chunk with its index.
type IndexedResultChunkData struct {
	Index       int
	ResultChunk types.ResultChunkData
}

type Store interface {
	Transact(ctx context.Context) (Store, error)
	Done(err error) error
	CreateTables(ctx context.Context) error
	Close(err error) error

	ReadMeta(ctx context.Context) (types.MetaData, error)
	PathsWithPrefix(ctx context.Context, prefix string) ([]string, error)
	ReadDocument(ctx context.Context, path string) (types.DocumentData, bool, error)
	ReadResultChunk(ctx context.Context, id int) (types.ResultChunkData, bool, error)
	ReadDefinitions(ctx context.Context, scheme, identifier string, skip, take int) ([]types.Location, int, error)
	ReadReferences(ctx context.Context, scheme, identifier string, skip, take int) ([]types.Location, int, error)

	WriteMeta(ctx context.Context, meta types.MetaData) error
	WriteDocuments(ctx context.Context, documents chan KeyedDocumentData) error
	WriteResultChunks(ctx context.Context, resultChunks chan IndexedResultChunkData) error
	WriteDefinitions(ctx context.Context, monikerLocations chan types.MonikerLocations) error
	WriteReferences(ctx context.Context, monikerLocations chan types.MonikerLocations) error
}

func DocumentsReferencing(ctx context.Context, s Store, paths []string) ([]string, error) {
	pathMap := map[string]struct{}{}
	for _, path := range paths {
		pathMap[path] = struct{}{}
	}

	meta, err := s.ReadMeta(ctx)
	if err != nil {
		return nil, err
	}

	resultIDs := map[types.ID]struct{}{}
	for i := 0; i < meta.NumResultChunks; i++ {
		resultChunk, exists, err := s.ReadResultChunk(ctx, i)
		if err != nil {
			return nil, err
		}
		if !exists {
			// TODO(efritz) - document that this should be fine
			continue
		}

		for resultID, documentIDRangeIDs := range resultChunk.DocumentIDRangeIDs {
			for _, documentIDRangeID := range documentIDRangeIDs {
				// Skip results that do not point into one of the given documents
				if _, ok := pathMap[resultChunk.DocumentPaths[documentIDRangeID.DocumentID]]; !ok {
					continue
				}

				resultIDs[resultID] = struct{}{}
			}
		}
	}

	allPaths, err := s.PathsWithPrefix(ctx, "")
	if err != nil {
		return nil, err
	}

	var pathsReferencing []string
	for _, path := range allPaths {
		document, exists, err := s.ReadDocument(ctx, path)
		if err != nil {
			return nil, err
		}
		if !exists {
			// TODO(efritz) - add error here - document should definitely exist
			continue
		}

		for _, r := range document.Ranges {
			if _, ok := resultIDs[r.DefinitionResultID]; ok {
				pathsReferencing = append(pathsReferencing, path)
				break
			}
		}
	}

	return pathsReferencing, nil
}
