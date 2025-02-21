package vfs

import (
	"testing"
	"time"

	"github.com/auula/wiredkv/types"
	"github.com/stretchr/testify/assert"
	"gopkg.in/mgo.v2/bson"
)

func TestNewSegment(t *testing.T) {
	// Test valid Set type
	set := types.Set{
		Set: map[string]bool{
			"item1": true,
			"item2": true,
		},
	}

	// Create a new segment for the Set type
	segment, err := NewSegment("mock-key", &set, 1000)
	assert.NoError(t, err)                                    // Ensure no error
	assert.NotNil(t, segment)                                 // Ensure segment is created
	assert.Equal(t, "mock-key", string(segment.Key))          // Ensure the key is set correctly
	assert.Equal(t, uint32(len("mock-key")), segment.KeySize) // Ensure the key size is correct
	assert.Equal(t, uint32(44), segment.ValueSize)            // Ensure the value size is correct
}

func TestNewTombstoneSegment(t *testing.T) {
	// Create a Tombstone segment
	segment := NewTombstoneSegment("mock-key")

	// Ensure the segment is of Tombstone type and has expected fields
	assert.Equal(t, Unknown, segment.Type)                    // Tombstone should have Unknown type
	assert.Equal(t, int8(1), segment.Tombstone)               // Tombstone should be marked as 1
	assert.Equal(t, "mock-key", string(segment.Key))          // Ensure the key is set correctly
	assert.Equal(t, uint32(len("mock-key")), segment.KeySize) // Ensure the key size is correct
}

func TestSegmentSize(t *testing.T) {
	// Create a Set type data for testing
	set := types.Set{
		Set: map[string]bool{
			"item1": true,
			"item2": true,
		},
	}

	// Create a segment for the Set type
	segment, err := NewSegment("mock-key", &set, 1000)
	assert.NoError(t, err)

	// Ensure the size is calculated correctly
	assert.Equal(t, uint32(82), segment.Size())
}

func TestToSet(t *testing.T) {
	// Create a Set type Segment
	setData := types.Set{
		Set: map[string]bool{
			"item1": true,
			"item2": true,
		},
		TTL: uint64(0),
	}
	segment, err := NewSegment("mock-key", &setData, 1000)
	assert.NoError(t, err)

	// Convert the segment to Set
	set, err := segment.ToSet()
	assert.NoError(t, err)                // Ensure no error
	assert.Equal(t, setData.Set, set.Set) // Ensure the Set values match
}

func TestTTL(t *testing.T) {
	// Create a Segment with TTL
	set := types.Set{
		Set: map[string]bool{
			"item1": true,
			"item2": true,
		},
	}
	segment, err := NewSegment("mock-key", &set, 1) // TTL = 1 second
	assert.NoError(t, err)

	// Wait 1 second
	time.Sleep(time.Second)

	// Test TTL, it should return a value close to 0
	ttl := segment.TTL()
	assert.True(t, ttl <= 0) // Ensure TTL is <= 0 after expiration
}

// TestToZSet 测试 ToZSet 方法
func TestToZSet(t *testing.T) {
	// 创建 ZSet 数据
	zsetData := types.ZSet{
		ZSet: map[string]float64{
			"user1": 100.5,
			"user2": 200.0,
		},
	}

	// 将 ZSet 序列化为 BSON
	data, err := bson.Marshal(zsetData)
	assert.NoError(t, err)

	// 构造 Segment
	segment := Segment{
		Type:  ZSet,
		Value: data,
	}

	// 测试 ToZSet 方法
	result, err := segment.ToZSet()
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, zsetData.ZSet, result.ZSet)
}

// TestToText 测试 ToText 方法
func TestToText(t *testing.T) {
	// 创建 Text 数据
	textData := types.Text{
		Content: "Hello, World!",
	}

	// 将 Text 序列化为 BSON
	data, err := bson.Marshal(textData)
	assert.NoError(t, err)

	// 构造 Segment
	segment := &Segment{
		Type:  Text,
		Value: data,
	}

	// 测试 ToText 方法
	result, err := segment.ToText()
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, textData.Content, result.Content)
}

// TestToList 测试 ToList 方法
func TestToList(t *testing.T) {
	// 创建 List 数据
	listData := types.List{
		List: []interface{}{"item1", "item2", 123},
	}

	// 将 List 序列化为 BSON
	data, err := bson.Marshal(listData)
	assert.NoError(t, err)

	// 构造 Segment
	segment := &Segment{
		Type:  List,
		Value: data,
	}

	// 测试 ToList 方法
	result, err := segment.ToList()
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, listData.List, result.List)
}

// TestToTables 测试 ToTables 方法
func TestToTables(t *testing.T) {
	// 创建 Tables 数据
	tablesData := types.Tables{
		Table: map[string]interface{}{
			"key1": "value1",
			"key2": 42,
		},
	}

	// 将 Tables 序列化为 BSON
	data, err := bson.Marshal(tablesData)
	assert.NoError(t, err)

	// 构造 Segment
	segment := &Segment{
		Type:  Tables,
		Value: data,
	}

	// 测试 ToTables 方法
	result, err := segment.ToTables()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	assert.Equal(t, tablesData.Table, result.Table)
}
