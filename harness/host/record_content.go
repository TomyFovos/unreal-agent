package host

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

type recordContent struct {
	io.ReaderAt
	size int64
	file *os.File
	data []byte
}

func memoryRecord(data []byte) recordContent {
	return recordContent{ReaderAt: bytes.NewReader(data), size: int64(len(data)), data: data}
}

func fileRecord(file *os.File, size int64) (recordContent, error) {
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || size >= 0 && size != info.Size()) {
		err = fmt.Errorf("%w: recorded file type or size changed", record.ErrUnavailable)
	}
	if err != nil {
		file.Close()
		return recordContent{}, err
	}
	return recordContent{ReaderAt: file, size: info.Size(), file: file}, nil
}

func (c recordContent) close() {
	if c.file != nil {
		c.file.Close()
	}
}

func (c recordContent) page(ctx context.Context, a record.Address, pageBytes int64) (record.Page, error) {
	selected := record.ByteRange{End: c.size}
	if a.Range != nil {
		selected = *a.Range
	} else if a.Line != 0 {
		var err error
		selected, err = c.line(ctx, a.Line)
		if err != nil {
			return record.Page{}, err
		}
	}
	if selected.End > c.size {
		return record.Page{}, fmt.Errorf("record byte range exceeds its %d bytes", c.size)
	}
	size := selected.End - selected.Start
	if pageBytes > 0 {
		size = min(size, pageBytes)
	}
	data, err := io.ReadAll(io.NewSectionReader(c, selected.Start, size))
	if err != nil {
		return record.Page{}, err
	}
	if int64(len(data)) != size {
		return record.Page{}, io.ErrUnexpectedEOF
	}
	page := record.Page{Bytes: data}
	if selected.Start+size < selected.End {
		a.Line = 0
		a.Range = &record.ByteRange{Start: selected.Start + size, End: selected.End}
		page.Continue = a.Locator()
	}
	return page, context.Cause(ctx)
}

func (c recordContent) line(ctx context.Context, number int64) (record.ByteRange, error) {
	input := bufio.NewReader(io.NewSectionReader(c, 0, c.size))
	var end, start int64
	for line := int64(1); start < c.size; {
		if err := context.Cause(ctx); err != nil {
			return record.ByteRange{}, err
		}
		part, err := input.ReadSlice('\n')
		end += int64(len(part))
		if err == bufio.ErrBufferFull && end < c.size {
			continue
		}
		if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
			return record.ByteRange{}, err
		}
		if len(part) == 0 && err == io.EOF {
			return record.ByteRange{}, io.ErrUnexpectedEOF
		}
		if line == number {
			return record.ByteRange{Start: start, End: end}, nil
		}
		line++
		start = end
	}
	return record.ByteRange{}, fs.ErrNotExist
}

func (c recordContent) contains(ctx context.Context, query []byte) (bool, error) {
	if int64(len(query)) > c.size {
		return false, nil
	}
	if c.file == nil {
		return bytes.Contains(c.data, query), nil
	}
	input := io.NewSectionReader(c, 0, c.size)
	// Keep enough of the previous read to match across buffer boundaries.
	buffer := make([]byte, max(32*1024, len(query)))
	kept := 0
	for {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		n, err := input.Read(buffer[kept:])
		end := kept + n
		if bytes.Contains(buffer[:end], query) {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		kept = min(end, len(query)-1)
		copy(buffer, buffer[end-kept:end])
	}
}
