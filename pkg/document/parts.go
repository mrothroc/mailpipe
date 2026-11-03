package document

import (
	"context"
	"io"
)

// InspectablePart is an ordered attachment or inline image. Path describes its
// position in the MIME tree, never a filesystem path. Data is transfer-decoded
// without charset conversion and remains owned by its FileDoc.
type InspectablePart struct {
	Path, Kind, Filename, ContentType, Disposition, CID string
	Data                                                *Blob
}

// ParsePartsReader uses the shared bounded parser, retaining all inline images,
// including images without names or Content-IDs. Providers must supply complete
// source via OpenSource first. Close the document before accepting a result.
func ParsePartsReader(ctx context.Context, r io.Reader, s *Scope) (*FileDoc, error) {
	return parseReader(ctx, r, s, true)
}

func (p *parser) part(path, kind, name, ct, disp, cid string, data *Blob) {
	if p.inspect {
		p.d.Parts = append(p.d.Parts, InspectablePart{path, kind, name, ct, disp, cid, data})
	}
}
