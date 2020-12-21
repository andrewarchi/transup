package pgs

import (
	"encoding/binary"
	"fmt"
	"io"
)

type SegmentReader struct {
	r io.Reader
}

func NewSegmentReader(r io.Reader) *SegmentReader {
	return &SegmentReader{r}
}

func (sr *SegmentReader) ReadSegment() (*Segment, error) {
	var h header
	if err := binary.Read(sr.r, binary.BigEndian, &h); err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("segment header: %w", err)
	}
	if h.MagicNumber != 0x5047 {
		return nil, fmt.Errorf(`magic number not "PG" 0x5047: %x`, h.MagicNumber)
	}
	switch h.SegmentType {
	case pcsType, wdsType, pdsType, odsType, endType:
	default:
		return nil, fmt.Errorf("unrecognized segment type: 0x%x", h.SegmentType)
	}
	s := &Segment{
		PresentationTime: h.PresentationTime.Duration(),
		DecodingTime:     h.DecodingTime.Duration(),
	}

	switch h.SegmentType {
	case pcsType:
		pc, err := sr.readPresentationComposition(h.SegmentSize)
		if err != nil {
			return nil, fmt.Errorf("presentation composition segment: %w", err)
		}
		s.Data = pc
	case wdsType:
		w, err := sr.readWindows(h.SegmentSize)
		if err != nil {
			return nil, fmt.Errorf("window definition segment: %w", err)
		}
		s.Data = w
	case pdsType:
		p, err := sr.readPalette(h.SegmentSize)
		if err != nil {
			return nil, fmt.Errorf("palette definition segment: %w", err)
		}
		s.Data = p
	case odsType:
		o, err := sr.readObject(h.SegmentSize)
		if err != nil {
			return nil, fmt.Errorf("object definition segment: %w", err)
		}
		s.Data = o
	case endType:
	default:
		panic(fmt.Sprintf("illegal: %d", h.SegmentType))
	}
	return s, nil
}

func (sr *SegmentReader) readPresentationComposition(segmentSize uint16) (*PresentationComposition, error) {
	var pcs pcs
	if err := binary.Read(sr.r, binary.BigEndian, &pcs); err != nil {
		return nil, err
	}
	switch pcs.CompositionState {
	case Normal, AcquisitionPoint, EpochStart:
	default:
		return nil, fmt.Errorf("unrecognized composition state: 0x%x", pcs.CompositionState)
	}
	if pcs.PaletteUpdateFlag&^pufTrue != 0 {
		return nil, fmt.Errorf("unrecognized palette update flag: 0x%x", pcs.PaletteUpdateFlag)
	}
	size := 11
	objects := make([]CompositionObject, pcs.ObjectCount)
	for i := range objects {
		var obj pcsCompositionObject
		if err := binary.Read(sr.r, binary.BigEndian, &obj); err != nil {
			return nil, fmt.Errorf("composition object %d/%d: %w", i+1, pcs.ObjectCount, err)
		}
		objects[i] = CompositionObject{
			ObjectID: obj.ObjectID,
			WindowID: obj.WindowID,
			X:        obj.X,
			Y:        obj.Y,
		}
		if obj.ObjectCropped&^croppedForce != 0 {
			return nil, fmt.Errorf("composition object %d/%d: unrecognized flag: 0x%x",
				i+1, pcs.ObjectCount, obj.ObjectCropped)
		}
		if obj.ObjectCropped == croppedForce {
			var crop CompositionObjectCrop
			if err := binary.Read(sr.r, binary.BigEndian, &crop); err != nil {
				return nil, err
			}
			objects[i].Crop = &crop
			size += 8
		}
		size += 8
	}
	if size != int(segmentSize) {
		return nil, fmt.Errorf("read %d bytes, %d bytes declared in header", size, segmentSize)
	}
	pc := &PresentationComposition{
		Width:             pcs.Width,
		Height:            pcs.Height,
		FrameRate:         pcs.FrameRate,
		CompositionNumber: pcs.CompositionNumber,
		CompositionState:  pcs.CompositionState,
		PaletteUpdate:     pcs.PaletteUpdateFlag&pufTrue != 0,
		PaletteID:         pcs.PaletteID,
		Objects:           objects,
	}
	return pc, nil
}

func (sr *SegmentReader) readWindows(segmentSize uint16) ([]Window, error) {
	var wds wds
	if err := binary.Read(sr.r, binary.BigEndian, &wds); err != nil {
		return nil, err
	}
	if segmentSize != uint16(wds.WindowCount)*9+1 {
		return nil, fmt.Errorf("segment size %d indicates %d windows, but %d specified",
			segmentSize, uint16(wds.WindowCount)*9+1, wds.WindowCount)
	}
	windows := make([]Window, wds.WindowCount)
	for i := range windows {
		if err := binary.Read(sr.r, binary.BigEndian, &windows[i]); err != nil {
			return nil, err
		}
	}
	return windows, nil
}

func (sr *SegmentReader) readPalette(segmentSize uint16) (*Palette, error) {
	if segmentSize%5 != 2 {
		return nil, fmt.Errorf("invalid segment size of %d bytes in header", segmentSize)
	}
	var pds pds
	if err := binary.Read(sr.r, binary.BigEndian, &pds); err != nil {
		return nil, err
	}
	n := (segmentSize - 2) / 5
	entries := make([]PaletteEntry, n)
	ids := make(map[uint8]struct{}, n)
	for i := range entries {
		if err := binary.Read(sr.r, binary.BigEndian, &entries[i]); err != nil {
			return nil, fmt.Errorf("palette entry %d/%d: %w", i, n, err)
		}
		id := entries[i].ID
		if _, ok := ids[id]; ok {
			return nil, fmt.Errorf("palette entry %d/%d: ID %d reused", i, n, id)
		}
		ids[id] = struct{}{}
	}
	p := &Palette{
		ID:      pds.PaletteID,
		Version: pds.PaletteVersion,
		Entries: entries,
	}
	return p, nil
}

func (sr *SegmentReader) readObject(segmentSize uint16) (*Object, error) {
	var ods ods
	if err := binary.Read(sr.r, binary.BigEndian, &ods); err != nil {
		return nil, err
	}
	if ods.SequenceFlag&^(firstInSequence|lastInSequence) != 0 {
		return nil, fmt.Errorf("unrecognized flag: 0x%x", ods.SequenceFlag)
	}
	dataLen := int(ods.ObjectDataLength.Uint32()) - 4
	if dataLen < 0 {
		return nil, fmt.Errorf("data length excludes width and height")
	}
	data := make([]byte, dataLen)
	n := 0
	for n < dataLen {
		n0, err := sr.r.Read(data[n:])
		if err != nil {
			return nil, err
		}
		n += n0
	}
	obj := &Object{
		ID:         ods.ObjectID,
		Version:    ods.ObjectVersion,
		First:      ods.SequenceFlag&firstInSequence != 0,
		Last:       ods.SequenceFlag&lastInSequence != 0,
		Width:      ods.Width,
		Height:     ods.Height,
		ObjectData: data,
	}
	return obj, nil
}