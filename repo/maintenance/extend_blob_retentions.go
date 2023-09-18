package maintenance

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
)

// DropDeletedContents rewrites indexes while dropping deleted contents above certain age.
func ExtendBlobRetention(ctx context.Context, rep repo.DirectRepositoryWriter, dropDeletedBefore time.Time, safety SafetyParameters) ([]blob.Metadata, error) {
	allBlobs, err := blob.ListAllBlobs(ctx, rep.BlobStorage(), "")
	if err != nil {
		return nil, errors.Wrap(err, "error listing blobs")
	}

	// Filter blob already updated today

	// Extend retention date

	for i, blob := range allBlobs {

	}
}
