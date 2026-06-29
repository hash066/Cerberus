package node

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

func ExecuteHelloShard(wasm []byte) (int, error) {
	module := wasmModule{r: bytes.NewReader(wasm), exportFunc: -1}
	if err := module.parse(); err != nil {
		return 0, err
	}
	if module.exportFunc < 0 {
		return 0, fmt.Errorf("missing %q export", HelloShardExport)
	}
	if module.exportFunc >= len(module.funcTypeIndexes) {
		return 0, fmt.Errorf("exported function index %d out of range", module.exportFunc)
	}
	if module.exportFunc >= len(module.codeBodies) {
		return 0, fmt.Errorf("missing code body for function %d", module.exportFunc)
	}
	return evalI32Const(module.codeBodies[module.exportFunc])
}

type wasmModule struct {
	r               *bytes.Reader
	exportFunc      int
	funcTypeIndexes []uint32
	codeBodies      [][]byte
}

func (m *wasmModule) parse() error {
	header := make([]byte, 8)
	if _, err := m.r.Read(header); err != nil {
		return err
	}
	if !bytes.Equal(header, []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}) {
		return errors.New("invalid wasm header")
	}
	for m.r.Len() > 0 {
		sectionID, err := m.r.ReadByte()
		if err != nil {
			return err
		}
		size, err := binary.ReadUvarint(m.r)
		if err != nil {
			return err
		}
		if uint64(m.r.Len()) < size {
			return errors.New("truncated wasm section")
		}
		payload := make([]byte, size)
		if _, err := m.r.Read(payload); err != nil {
			return err
		}
		if err := m.parseSection(sectionID, payload); err != nil {
			return err
		}
	}
	return nil
}

func (m *wasmModule) parseSection(sectionID byte, payload []byte) error {
	reader := bytes.NewReader(payload)
	switch sectionID {
	case 3:
		count, err := binary.ReadUvarint(reader)
		if err != nil {
			return err
		}
		m.funcTypeIndexes = make([]uint32, 0, count)
		for i := uint64(0); i < count; i++ {
			typeIndex, err := binary.ReadUvarint(reader)
			if err != nil {
				return err
			}
			m.funcTypeIndexes = append(m.funcTypeIndexes, uint32(typeIndex))
		}
	case 7:
		count, err := binary.ReadUvarint(reader)
		if err != nil {
			return err
		}
		for i := uint64(0); i < count; i++ {
			name, err := readName(reader)
			if err != nil {
				return err
			}
			kind, err := reader.ReadByte()
			if err != nil {
				return err
			}
			index, err := binary.ReadUvarint(reader)
			if err != nil {
				return err
			}
			if name == HelloShardExport && kind == 0x00 {
				m.exportFunc = int(index)
			}
		}
	case 10:
		count, err := binary.ReadUvarint(reader)
		if err != nil {
			return err
		}
		m.codeBodies = make([][]byte, 0, count)
		for i := uint64(0); i < count; i++ {
			bodySize, err := binary.ReadUvarint(reader)
			if err != nil {
				return err
			}
			if uint64(reader.Len()) < bodySize {
				return errors.New("truncated code body")
			}
			body := make([]byte, bodySize)
			if _, err := reader.Read(body); err != nil {
				return err
			}
			m.codeBodies = append(m.codeBodies, body)
		}
	}
	return nil
}

func evalI32Const(body []byte) (int, error) {
	reader := bytes.NewReader(body)
	localGroupCount, err := binary.ReadUvarint(reader)
	if err != nil {
		return 0, err
	}
	for i := uint64(0); i < localGroupCount; i++ {
		if _, err := binary.ReadUvarint(reader); err != nil {
			return 0, err
		}
		if _, err := reader.ReadByte(); err != nil {
			return 0, err
		}
	}
	opcode, err := reader.ReadByte()
	if err != nil {
		return 0, err
	}
	if opcode != 0x41 {
		return 0, fmt.Errorf("unsupported opcode 0x%x", opcode)
	}
	value, err := readSignedLEB(reader, 32)
	if err != nil {
		return 0, err
	}
	end, err := reader.ReadByte()
	if err != nil {
		return 0, err
	}
	if end != 0x0b {
		return 0, errors.New("wasm function does not end cleanly")
	}
	if reader.Len() != 0 {
		return 0, errors.New("unsupported trailing wasm instructions")
	}
	return int(value), nil
}

func readSignedLEB(reader *bytes.Reader, size uint) (int64, error) {
	var result int64
	var shift uint
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= int64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			if shift < size && b&0x40 != 0 {
				result |= ^0 << shift
			}
			return result, nil
		}
		if shift >= size+7 {
			return 0, errors.New("signed LEB value is too large")
		}
	}
}

func readName(reader *bytes.Reader) (string, error) {
	size, err := binary.ReadUvarint(reader)
	if err != nil {
		return "", err
	}
	if uint64(reader.Len()) < size {
		return "", errors.New("truncated name")
	}
	name := make([]byte, size)
	if _, err := reader.Read(name); err != nil {
		return "", err
	}
	return string(name), nil
}
