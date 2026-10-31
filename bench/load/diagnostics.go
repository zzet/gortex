package main

import "fmt"

const diagnosticHalf = 4096

// Keep both the initial context and final fatal message without retaining an
// unbounded command's stderr. Stdout remains separate for successful Git reads.
type diagnosticBuffer struct {
	head, tail []byte
	omitted    int
}

func (b *diagnosticBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if available := diagnosticHalf - len(b.head); available > 0 {
		count := min(available, len(data))
		b.head = append(b.head, data[:count]...)
		data = data[count:]
	}
	if len(data) >= diagnosticHalf {
		b.omitted += len(b.tail) + len(data) - diagnosticHalf
		b.tail = append(b.tail[:0], data[len(data)-diagnosticHalf:]...)
	} else {
		if extra := len(b.tail) + len(data) - diagnosticHalf; extra > 0 {
			b.omitted += extra
			b.tail = b.tail[:copy(b.tail, b.tail[extra:])]
		}
		b.tail = append(b.tail, data...)
	}
	return n, nil
}

func (b *diagnosticBuffer) String() string {
	if b.omitted == 0 {
		return string(b.head) + string(b.tail)
	}
	return fmt.Sprintf("%s\n... %d diagnostic bytes omitted ...\n%s", b.head, b.omitted, b.tail)
}
