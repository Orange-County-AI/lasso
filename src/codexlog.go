package main

import (
	"bufio"
	"bytes"
	"io"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// Codex's session log cannot be read the way omp's and claude's are.
//
// Codex inlines every image a tool hands back as a base64 data URL inside the
// JSONL record, so one line routinely runs to tens of megabytes: a 408 MB log
// on titan carried lines of 44 MB. The chat's tail window (chatReadBytes)
// assumes a record is small next to the window. Here one record is a hundred
// windows, so the tail lands inside an image and shows nothing, and paging back
// from it can never get past it.
//
// So this log is read LINE by line. Each line streams through elider, which
// cuts any string past codexStringCap down to a prefix (a data URL down to
// nothing), so a 44 MB tool result arrives as the few hundred bytes the chat
// actually renders. Where the lines are is remembered per file (codexLog): a
// session log is append-only, so a line's bytes never change once written, and
// a giant line is scanned once per process instead of on every 2s poll. Over
// SFTP that is the difference between a chat and a stall.

const (
	// codexStringCap is the longest string kept from a record. Far past anything
	// a card shows (chatOutputCap), well short of an inlined image.
	codexStringCap = 8 << 10
	// codexScanChunk is one backwards read while looking for a line start.
	codexScanChunk = 256 << 10
	// codexScanMax bounds the raw bytes one request may read. Past it the page
	// ends where the scan got to and reports more above, so an image-heavy stretch
	// costs a short page rather than a long stall (over SFTP, seconds per 100 MB).
	// One line is always read whole however long it is, or a line past the bound
	// could never be got past; codexLineMax is the sanity limit on that.
	codexScanMax = 64 << 20
	codexLineMax = 1 << 30
	// codexCacheKept bounds the compacted lines one file's cache may hold, and
	// codexCacheFiles how many files are cached at once.
	codexCacheKept  = 32 << 20
	codexCacheFiles = 8
)

// jsonlLine is one complete log line, compacted, and where it starts in the
// file. The offset is what rows are stamped with, so paging stays exact even
// though the bytes are no longer the file's.
type jsonlLine struct {
	off  int64
	data []byte
}

// codexLog is what is known about one log file: the lines covering [lo, hi),
// both of them line boundaries.
type codexLog struct {
	mu    sync.Mutex
	init  bool
	lo    int64
	hi    int64
	lines []jsonlLine
	kept  int
	used  time.Time
}

var codexLogs = struct {
	sync.Mutex
	m map[string]*codexLog
}{m: map[string]*codexLog{}}

func codexLogFor(host, path string) *codexLog {
	codexLogs.Lock()
	defer codexLogs.Unlock()
	key := host + "\x00" + path
	c, ok := codexLogs.m[key]
	if !ok {
		for len(codexLogs.m) >= codexCacheFiles {
			oldest, at := "", time.Time{}
			for k, v := range codexLogs.m {
				if oldest == "" || v.used.Before(at) {
					oldest, at = k, v.used
				}
			}
			delete(codexLogs.m, oldest)
		}
		c = &codexLog{}
		codexLogs.m[key] = c
	}
	c.used = time.Now()
	return c
}

// codexLinesBefore returns the complete lines that end at or before `end`,
// newest last, walking back until `budget` compacted bytes are collected or the
// file's start is reached.
func codexLinesBefore(b Backend, path string, size, end int64, budget int) []jsonlLine {
	c := codexLogFor(b.Name(), path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sync(b, path, size) {
		return nil
	}
	if end > c.hi {
		end = c.hi
	}
	scanned := int64(0)
	for {
		// Lines are contiguous, so a line ends at or before `end` exactly when the
		// NEXT one starts at or before it.
		i := sort.Search(len(c.lines), func(i int) bool { return c.lineEnd(i) > end })
		kept, first := 0, i
		for first > 0 && kept < budget {
			first--
			kept += len(c.lines[first].data)
		}
		if kept >= budget || c.lo == 0 || scanned >= codexScanMax {
			return append([]jsonlLine(nil), c.lines[first:i]...)
		}
		n := c.back(b, path)
		if n == 0 {
			return append([]jsonlLine(nil), c.lines[first:i]...)
		}
		scanned += n
	}
}

// codexLinesFrom returns the complete lines starting at or after `from`, up to
// `budget` compacted bytes: where a result is looked for past a page's end.
func codexLinesFrom(b Backend, path string, size, from int64, budget int) []jsonlLine {
	c := codexLogFor(b.Name(), path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sync(b, path, size) {
		return nil
	}
	for scanned := int64(0); c.lo > from && scanned < codexScanMax; {
		n := c.back(b, path)
		if n == 0 {
			break
		}
		scanned += n
	}
	i := sort.Search(len(c.lines), func(i int) bool { return c.lines[i].off >= from })
	var out []jsonlLine
	for kept := 0; i < len(c.lines) && kept < budget; i++ {
		out = append(out, c.lines[i])
		kept += len(c.lines[i].data)
	}
	return out
}

func (c *codexLog) lineEnd(i int) int64 {
	if i+1 < len(c.lines) {
		return c.lines[i+1].off
	}
	return c.hi
}

// sync brings the cache up to the file as it now is: anchored on first use,
// dropped if the file shrank (a new file at the same path) or the cache grew
// past its bound, and extended over whatever was appended since.
func (c *codexLog) sync(b Backend, path string, size int64) bool {
	if c.init && (size < c.hi || c.kept > codexCacheKept) {
		*c = codexLog{used: c.used}
	}
	if !c.init {
		anchor, ok := lastLineEnd(b, path, size)
		if !ok {
			return false
		}
		c.lo, c.hi, c.init = anchor, anchor, true
	}
	if size > c.hi {
		lines, next, err := readLines(b, path, c.hi, size)
		if err != nil {
			return false
		}
		for _, l := range lines {
			c.kept += len(l.data)
		}
		c.lines = append(c.lines, lines...)
		c.hi = next
	}
	return true
}

// back prepends the lines before lo. It reads backwards until it finds a line
// start, then streams forwards over the lines it found. Returns the raw bytes
// read, zero when there was nothing more to read or the read failed.
func (c *codexLog) back(b Backend, path string) int64 {
	if c.lo == 0 {
		return 0
	}
	f, err := b.Open(path)
	if err != nil {
		return 0
	}
	// The byte at lo-1 is the newline ending the previous line: look before it.
	pos, start, read := c.lo-1, int64(-1), int64(0)
	buf := make([]byte, codexScanChunk)
	for start < 0 {
		from := pos - codexScanChunk
		if from < 0 {
			from = 0
		}
		chunk := buf[:pos-from]
		_, err := f.Seek(from, io.SeekStart)
		if err == nil {
			_, err = io.ReadFull(f, chunk)
		}
		if err != nil {
			f.Close()
			return 0
		}
		read += int64(len(chunk))
		// The FIRST newline in the chunk: every line after it is complete, which
		// takes several short lines in one step instead of one per scan.
		if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
			start = from + int64(i) + 1
		} else if from == 0 {
			start = 0
		}
		pos = from
		if read > codexLineMax {
			f.Close()
			return 0
		}
	}
	f.Close()
	oldLo := c.lo
	lines, _, err := readLines(b, path, start, oldLo)
	if err != nil {
		return 0
	}
	kept := 0
	for _, l := range lines {
		kept += len(l.data)
	}
	c.lines = append(lines, c.lines...)
	c.kept += kept
	c.lo = start
	return read + (oldLo - start)
}

// lastLineEnd is the offset just past the last newline before size: the end of
// the last complete line. A line still being written is not one yet.
func lastLineEnd(b Backend, path string, size int64) (int64, bool) {
	f, err := b.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, codexScanChunk)
	for pos, read := size, int64(0); pos > 0; {
		if read > codexLineMax {
			return 0, false
		}
		from := pos - codexScanChunk
		if from < 0 {
			from = 0
		}
		chunk := buf[:pos-from]
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return 0, false
		}
		if _, err := io.ReadFull(f, chunk); err != nil {
			return 0, false
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return from + int64(i) + 1, true
		}
		read += int64(len(chunk))
		pos = from
	}
	return 0, true
}

// readLines streams [from, to) and returns its complete lines, compacted, plus
// where the last one ended (a trailing partial line is left for later).
func readLines(b Backend, path string, from, to int64) ([]jsonlLine, int64, error) {
	f, err := b.Open(path)
	if err != nil {
		return nil, from, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, from, err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, to-from), 64<<10)
	var (
		out   []jsonlLine
		e     elider
		start = from
		pos   = from
	)
	for {
		chunk, err := r.ReadSlice('\n')
		pos += int64(len(chunk))
		if n := len(chunk); n > 0 && chunk[n-1] == '\n' {
			e.write(chunk[:n-1])
			out = append(out, jsonlLine{off: start, data: e.take()})
			start = pos
		} else {
			e.write(chunk)
		}
		switch err {
		case nil, bufio.ErrBufferFull:
			continue
		case io.EOF:
			return out, start, nil
		default:
			return out, start, err
		}
	}
}

// elider copies JSON through, cutting every string longer than codexStringCap
// to a prefix of it. The prefix ends on a character boundary, never inside an
// escape or a UTF-8 sequence, so the output stays valid JSON; a data URL is cut
// to its bare scheme, since a prefix of base64 is worth nothing to a reader.
type elider struct {
	out   []byte
	str   []byte
	inStr bool
	over  bool
	esc   int // bytes of the current escape still to come
}

func (e *elider) write(p []byte) {
	for _, c := range p {
		if !e.inStr {
			e.out = append(e.out, c)
			if c == '"' {
				e.inStr, e.over, e.esc, e.str = true, false, 0, e.str[:0]
			}
			continue
		}
		if e.esc == 0 && c == '"' {
			s := e.str
			if e.over && bytes.HasPrefix(s, []byte("data:")) {
				s = s[:len("data:")]
			}
			e.out = append(e.out, s...)
			e.out = append(e.out, '"')
			e.inStr = false
			continue
		}
		boundary := e.esc == 0 && (c < utf8.RuneSelf || utf8.RuneStart(c))
		switch {
		case e.esc == 1 && c == 'u':
			e.esc = 4
		case e.esc > 0:
			e.esc--
		case c == '\\':
			e.esc = 1
		}
		if e.over {
			continue
		}
		if boundary && len(e.str) >= codexStringCap {
			e.over = true
			continue
		}
		e.str = append(e.str, c)
	}
}

// take returns the line written so far and resets for the next. A line that
// ended inside a string is not JSON anyway; it is closed off so the parser
// rejects it cleanly rather than reading a torn prefix.
func (e *elider) take() []byte {
	if e.inStr {
		e.out = append(e.out, e.str...)
	}
	line := e.out
	e.out, e.inStr, e.over, e.esc = nil, false, false, 0
	return line
}
