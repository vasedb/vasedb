package vfs

import (
	"errors"
	"fmt"
	"time"

	"github.com/auula/wiredkv/types"
)

type Kind int8

const (
	Set Kind = iota
	ZSet
	List
	Text
	Tables
	Binary
	Number
	Unknown
)

// | DEL 1 | KIND 1 | EAT 8 | CAT 8 | KLEN 8 | VLEN 8 | KEY ? | VALUE ? | CRC32 4 |
type Segment struct {
	Tombstone int8
	Type      Kind
	ExpiredAt uint64
	CreatedAt uint64
	KeySize   uint32
	ValueSize uint32
	Key       []byte
	Value     []byte
}

type Serializable interface {
	ToBSON() []byte
}

// NewSegment 使用数据类型初始化并返回对应的 Segment
func NewSegment(key string, data Serializable, ttl uint64) (*Segment, error) {
	kind, err := toKind(data)
	if err != nil {
		return nil, fmt.Errorf("unsupported data type: %w", err)
	}

	timestamp, expiredAt := uint64(time.Now().Unix()), uint64(0)
	if ttl > 0 {
		expiredAt = uint64(time.Now().Add(time.Second * time.Duration(ttl)).Unix())
	}

	// 这个是通过 transformer 编码之后的
	encodedata, err := transformer.Encode(data.ToBSON())
	if err != nil {
		return nil, fmt.Errorf("transformer encode: %w", err)
	}

	// 如果类型不匹配，则返回错误
	return &Segment{
		Type:      kind,
		Tombstone: 0,
		CreatedAt: timestamp,
		ExpiredAt: expiredAt,
		KeySize:   uint32(len(key)),
		ValueSize: uint32(len(encodedata)),
		Key:       []byte(key),
		Value:     encodedata,
	}, nil

}

func NewTombstoneSegment(key []byte) *Segment {
	seg := new(Segment)
	seg.Key = key
	seg.Tombstone = 1
	seg.KeySize = uint32(len(key))
	return seg
}

func (s *Segment) IsTombstone() bool {
	return s.Tombstone == 1
}

func (s *Segment) Size() uint32 {
	// 计算一整块记录的大小，+4 CRC 校验码占用 4 个字节
	return 26 + s.KeySize + s.ValueSize + 4
}

func (s *Segment) ToSet() *types.Set {
	if s.Type != Set {
		return nil
	}
	// 假设您的数据是 JSON 或某种结构体，可以进行反序列化
	var set types.Set
	// Deserialize s.data to set (具体根据类型定义来做反序列化)
	return &set
}

func (s *Segment) ToZSet() *types.ZSet {
	return nil
}

func (s *Segment) ToText() *types.Text {
	return nil
}

func (s *Segment) ToList() *types.List {
	return nil
}

func (s *Segment) ToTables() *types.Tables {
	return nil
}

func (s *Segment) ToBinary() *types.Binary {
	return nil
}

func (s *Segment) ToNumber() *types.Number {
	return nil
}

func (s *Segment) TTL() int64 {
	now := uint64(time.Now().Unix())
	if s.ExpiredAt > 0 && s.ExpiredAt > now {
		return int64(s.ExpiredAt - now)
	}
	return -1
}

// 将类型映射为 Kind 的辅助函数
func toKind(data Serializable) (Kind, error) {
	switch data.(type) {
	case *types.Set:
		return Set, nil
	case *types.ZSet:
		return ZSet, nil
	case *types.List:
		return List, nil
	case *types.Text:
		return Text, nil
	case *types.Tables:
		return Tables, nil
	case *types.Binary:
		return Binary, nil
	case *types.Number:
		return Number, nil
	default:
		return Unknown, errors.New("unknown data type")
	}
}
