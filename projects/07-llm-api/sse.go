package llm

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// sseEvent 是一条解析好的 SSE 事件。
type sseEvent struct {
	Event string // 没有 event: 行时为 "message"
	Data  string
	ID    string
}

// sseReader 按 0.4 课的规范解析 SSE：按行切（\n、\r\n、\r 都认），空行派发，冒号开头是注释。
// bufio.Reader 按字节读行，所以多字节 UTF-8 不会被劈开；一行最长 8 MB（工具参数可能很大）。
type sseReader struct {
	r      *bufio.Reader
	lastID string
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

const maxSSELine = 8 << 20

func (s *sseReader) readLine() (string, error) {
	var buf bytes.Buffer
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if err == io.EOF && buf.Len() > 0 {
				return buf.String(), nil
			}
			return "", err
		}
		switch b {
		case '\n':
			return buf.String(), nil
		case '\r':
			if nb, err := s.r.Peek(1); err == nil && nb[0] == '\n' {
				s.r.ReadByte()
			}
			return buf.String(), nil
		}
		if buf.Len() >= maxSSELine {
			return "", bufio.ErrTooLong
		}
		buf.WriteByte(b)
	}
}

// next 返回下一个有 data 的事件；流结束返回 io.EOF。
// 最后一个事件后面如果缺了空行就断开，那条事件不派发（规范如此），交给上层判断“没收到终止事件”。
func (s *sseReader) next() (sseEvent, error) {
	var (
		data    []string
		hasData bool
		event   string
	)
	for {
		line, err := s.readLine()
		if err != nil {
			return sseEvent{}, err
		}
		if line == "" {
			if !hasData {
				event = ""
				continue
			}
			if event == "" {
				event = "message"
			}
			return sseEvent{Event: event, Data: strings.Join(data, "\n"), ID: s.lastID}, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "data":
			data = append(data, value)
			hasData = true
		case "event":
			event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				s.lastID = value
			}
		}
	}
}
