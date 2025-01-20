package vfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/auula/wiredkv/clog"
	"github.com/auula/wiredkv/utils"
)

const RWCA = os.O_RDWR | os.O_CREATE | os.O_APPEND

const (
	_  = 1 << (10 * iota) // skip iota = 0
	KB                    // 2^10 = 1024
	MB                    // 2^20 = 1048576
	GB                    // 2^30 = 1073741824
)

type GC_STATUS = int8 // Region garbage collection status

const (
	GC_INIT GC_STATUS = iota // gc 第一次执行就是这个状态
	GC_STOP
	GC_RUNNING
)

var (
	indexShard       = 5
	instance         *LogStructuredFS
	fsPerm           = fs.FileMode(0755)
	fileExtension    = ".wdb"
	indexFileName    = "index.wdb"
	regionThreshold  = int64(1 * GB) // 1GB
	dataFileMetadata = []byte{0xDB, 0x0, 0x0, 0x1}
	transformer      = NewTransformer()
)

type Options struct {
	Path      string
	FsPerm    os.FileMode
	Threshold uint8 // 这个的大小会影响到垃圾回收执行的时间
}

// INode represents a file system node with metadata.
type INode struct {
	RegionID  uint64 // Unique identifier for the region
	Position  uint64 // Position within the file
	Length    uint32 // Data record length
	ExpiredAt uint64 // Expiration time of the INode (UNIX timestamp in seconds)
	CreatedAt uint64 // Creation time of the INode (UNIX timestamp in seconds)
}

type indexMap struct {
	mu    sync.RWMutex      // 每个分片使用独立的锁
	index map[uint64]*INode // 存储映射
}

// LogStructuredFS represents the virtual file storage system.
type LogStructuredFS struct {
	mu          sync.Mutex
	offset      uint64
	regionID    uint64
	directory   string
	indexs      []*indexMap
	active      *os.File
	regions     map[uint64]*os.File
	gcstate     GC_STATUS
	gcdone      chan struct{}
	dirtyRegion []*os.File
}

// AddSegment 会向 LogStructuredFS 虚拟文件系统插入一条 Segment 记录
func (lfs *LogStructuredFS) AddSegment(inum uint64, seg Segment, ttl uint64) error {
	// 根据某种哈希函数简单的模运算来选择索引分片
	shard := lfs.indexs[inum%uint64(indexShard)]

	err := appendBinaryToFile(lfs.active, &seg)
	if err != nil {
		lfs.mu.Unlock()
		return err
	}

	lfs.mu.Lock()
	inode := &INode{
		RegionID:  lfs.regionID,
		Position:  lfs.offset,
		Length:    seg.Size(),
		CreatedAt: seg.CreatedAt,
		ExpiredAt: seg.ExpiredAt,
	}
	lfs.offset += uint64(seg.Size())
	lfs.mu.Unlock()

	shard.mu.Lock()
	shard.index[inum] = inode
	shard.mu.Unlock()

	return nil
}

func (lfs *LogStructuredFS) GetINode(inum uint64) (*INode, bool) {
	shard := lfs.indexs[inum%uint64(indexShard)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	inode, exists := shard.index[inum]
	return inode, exists
}

func (lfs *LogStructuredFS) BatchINodes(inodes ...*INode) {

}

func InodeNum(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

func (lfs *LogStructuredFS) ChangeRegions() error {
	lfs.mu.Lock()
	defer lfs.mu.Unlock()

	err := lfs.active.Sync()
	if err != nil {
		return fmt.Errorf("failed to change active regions: %w", err)
	}

	lfs.regions[lfs.regionID] = lfs.active

	err = lfs.createActiveRegion()
	if err != nil {
		return fmt.Errorf("failed to chanage active regions: %w", err)
	}

	return nil
}

func (lfs *LogStructuredFS) createActiveRegion() error {
	lfs.regionID += 1
	fileName, err := generateFileName(lfs.regionID)
	if err != nil {
		return fmt.Errorf("failed to new active region name: %w", err)
	}

	active, err := os.OpenFile(filepath.Join(lfs.directory, fileName), RWCA, fsPerm)
	if err != nil {
		return fmt.Errorf("failed to create active region: %w", err)
	}

	n, err := active.Write(dataFileMetadata)
	if err != nil {
		return fmt.Errorf("failed to write active region metadata: %w", err)
	}

	if n != len(dataFileMetadata) {
		return errors.New("failed to active region metadata write")
	}

	lfs.active = active
	lfs.offset = uint64(len(dataFileMetadata))

	return nil
}

func (lfs *LogStructuredFS) recoverRegions() error {
	lfs.mu.Lock()
	defer lfs.mu.Unlock()
	files, err := os.ReadDir(lfs.directory)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), fileExtension) {
			if strings.HasPrefix(file.Name(), "0") {
				regions, err := os.OpenFile(filepath.Join(lfs.directory, file.Name()), os.O_RDWR, fsPerm)
				if err != nil {
					return fmt.Errorf("failed to open data file: %w", err)
				}

				regionID, err := parseDataFileName(file.Name())
				if err != nil {
					return fmt.Errorf("failed to get regions id: %w", err)
				}
				lfs.regions[regionID] = regions
			}
		}
	}

	// 只有数据文件大于 1 时才找到最大的那个文件
	if len(lfs.regions) >= 1 {
		var regionIds []uint64
		for v := range lfs.regions {
			regionIds = append(regionIds, v)
		}
		// 对 regionIds 切片从小到大排序
		sort.Slice(regionIds, func(i, j int) bool {
			return regionIds[i] < regionIds[j]
		})

		// 找到最新数据文件的版本
		lfs.regionID = regionIds[len(regionIds)-1]

		// 如果最大那个 region 文件没有达到阀值就不用创建新文件，如果大于就创建新的文件
		active, ok := lfs.regions[lfs.regionID]
		if !ok {
			return fmt.Errorf("region file not found for region id: %d", lfs.regionID)
		}
		stat, err := active.Stat()
		if err != nil {
			return fmt.Errorf("failed to get region file info: %w", err)
		}

		if stat.Size() >= regionThreshold {
			return lfs.createActiveRegion()
		} else {
			offset, err := active.Seek(0, io.SeekEnd)
			if err != nil {
				return fmt.Errorf("failed to get region file offset: %w", err)
			}
			lfs.active = active
			lfs.offset = uint64(offset)
		}
	} else {
		// 如果是空文件夹就创建的一个可写的数据文件
		return lfs.createActiveRegion()
	}

	return nil
}

// recoveryIndex 会对磁盘上的数据文件执行恢复索引操作，步骤如下：
// 读取索引文件快照文件，从快照文件里面恢复索引
// 不同于 bitcask 对 hint 文件是在 compressor 过程中生成
// bitcask 中 hint 文件是在压缩过程中生成 hint 快照
// 并不能代表全部即时内存索引状态
// vasedb 则完全设计了不同的方案，如果是 close 正常关闭的就会生成 index 文件
// 如果数据文件有 index 文件则直接从 index 文件中恢复
// 没有就在启动的全局扫描数据文件重新构建索引文件
func (lfs *LogStructuredFS) recoveryIndex() error {
	lfs.mu.Lock()
	defer lfs.mu.Unlock()
	// 构造完整的文件路径
	filePath := filepath.Join(lfs.directory, indexFileName)
	if utils.IsExist(filePath) {
		// 如果存在索引文件就恢复
		file, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("failed to open index file: %w", err)
		}
		defer file.Close()

		err = recoveryIndex(file, lfs.indexs)
		if err != nil {
			return fmt.Errorf("failed to recovery index mapping: %w", err)
		}

		return nil
	}

	// 如果不存在索引文件就从 regions 文件全局扫描恢复
	// 如果数据文件非常大，而且文件非常多，恢复多时间就越长
	// 如果垃圾回收越频繁，你数据文件就变小，启动时间就越快
	// 但是如果垃圾回收越频繁，可能会影响到整体数据读取写性能
	return crashRecoveryAllIndex(lfs.regions, lfs.indexs)
}

func (lfs *LogStructuredFS) SetCompressor(compressor Compressor) {
	transformer.SetCompressor(compressor)
}

func (lfs *LogStructuredFS) SetEncryptor(encryptor Encryptor, secret []byte) error {
	return transformer.SetEncryptor(encryptor, secret)
}

func (lfs *LogStructuredFS) StartRegionGC(cycle_second time.Duration) {
	if lfs.gcstate != GC_INIT {
		return
	}
	// 创建一个 ticker，每秒触发一次
	ticker := time.NewTicker(cycle_second)
	// 控制这个垃圾回收 goruntine 正常退出
	lfs.gcdone = make(chan struct{}, 1)
	// 启动一个 goroutine，不断接收 ticker 通道的消息
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// 上一个 gc 还在执行就跳过本周期的
				if lfs.gcstate == GC_RUNNING {
					continue
				}

				lfs.compressDirtyRegion()

				// 修改 gc 停止运行状态
				lfs.gcstate = GC_STOP
			case <-lfs.gcdone:
				// 如果 gc 正在运行延迟 gc 退出
				// 防止正在执行的 gc 就被中断了导致产生了脏数据
				for lfs.gcstate == GC_RUNNING {
					time.Sleep(3 * time.Second)
				}
				lfs.gcstate = GC_INIT
				return
			}
		}
	}()
}

func (lfs *LogStructuredFS) StopRegionGC() {
	if lfs.gcstate == GC_RUNNING || lfs.gcstate == GC_STOP {
		lfs.gcdone <- struct{}{}
		close(lfs.gcdone)
	}
}

func (lfs *LogStructuredFS) RegionGCStatus() GC_STATUS {
	return lfs.gcstate
}

func OpenFS(opt *Options) (*LogStructuredFS, error) {
	// single region max size = 255GB
	regionThreshold = int64(opt.Threshold) * GB

	err := checkFileSystem(opt.Path)
	if err != nil {
		return nil, err
	}

	fsPerm = opt.FsPerm
	instance = &LogStructuredFS{
		indexs:    make([]*indexMap, indexShard),
		regions:   make(map[uint64]*os.File, 10),
		offset:    uint64(len(dataFileMetadata)),
		regionID:  0,
		directory: opt.Path,
		gcstate:   GC_INIT,
	}

	for i := 0; i < indexShard; i++ {
		instance.indexs[i] = &indexMap{
			mu:    sync.RWMutex{},
			index: make(map[uint64]*INode, 100000),
		}
	}

	// 先对已有的数据文件执行恢复操作，并且初始化内存中的数据版本号
	err = instance.recoverRegions()
	if err != nil {
		return nil, fmt.Errorf("failed to recover data regions: %w", err)
	}

	err = instance.recoveryIndex()
	if err != nil {
		return nil, fmt.Errorf("failed to recover regions index: %w", err)
	}

	// 单例子模式，但是挡不住其他包通过 new(LogStructuredFS) 也能创建一个实例，那这样根本不起作用了
	return instance, nil
}

// 关闭之前一定要检查 gc 是否在执行，如果 gc 在执行千万不要盲目的关闭
func (lfs *LogStructuredFS) CloseFS() error {
	lfs.mu.Lock()
	defer lfs.mu.Unlock()
	for _, file := range lfs.regions {
		err := utils.CloseFile(file)
		if err != nil {
			return fmt.Errorf("failed to close region file: %w", err)
		}
	}

	// 如果有 index 文件的快照，就从 index 文件快照进行恢复，如果没有就全局扫描
	return lfs.ExportSnapshotIndex()
}

// ExportSnapshotIndex 是正常程序退出是所做的操作，导出内存索引快照到磁盘文件
// 当前的设计方案对于内存资源较少的系统有限制，
// 例如 RAM 512 MB < 1GB，如果 1GB 快照不能全部序列化到磁盘上，
// 映射大文件到内存可能不是一个好的选择，因为它会占用大量的虚拟内存空间，会出现 swap 交换内存页。
func (lfs *LogStructuredFS) ExportSnapshotIndex() error {
	filePath := filepath.Join(lfs.directory, indexFileName)
	fd, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, fsPerm)
	if err != nil {
		return fmt.Errorf("failed to generate index snapshot file: %w", err)
	}
	defer utils.CloseFile(fd)

	// 写入元数据
	n, err := fd.Write(dataFileMetadata)
	if err != nil {
		return fmt.Errorf("failed to write index file metadata: %w", err)
	}

	if n != len(dataFileMetadata) {
		return errors.New("index file metadata write incomplete")
	}

	// 遍历分片索引并写入
	for _, indexs := range lfs.indexs {
		indexs.mu.RLock()
		defer indexs.mu.RUnlock()
		for inum, inode := range indexs.index {
			bytes, err := serializedIndex(inum, inode)
			if err != nil {
				return fmt.Errorf("failed to serialized index (inum: %d): %w", inum, err)
			}
			_, err = fd.Write(bytes)
			if err != nil {
				return fmt.Errorf("failed to write serialized index (inum: %d): %w", inum, err)
			}
		}
	}

	return nil
}

func recoveryIndex(fd *os.File, indexs []*indexMap) error {
	// 在恢复操作的时候不需要上锁
	offset := int64(len(dataFileMetadata))

	finfo, err := fd.Stat()
	if err != nil {
		return err
	}

	type index struct {
		inum  uint64
		inode *INode
	}

	// 共享并行处理的数据
	nqueue := make(chan index, (finfo.Size()-offset)/48)
	// 定义一个错误通道，用于捕获 goroutine 中的错误
	// 只捕获到第一个错误，一旦有错误全局停止直接返回
	equeue := make(chan error, 1)

	// 定义一个 WaitGroup 来等待所有 goroutine 完成
	var wg sync.WaitGroup

	// 生产者 goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(nqueue)

		for offset < finfo.Size() && len(equeue) == 0 {
			buf := make([]byte, 48)
			_, err := fd.ReadAt(buf, offset)
			if err != nil {
				equeue <- fmt.Errorf("failed to read index node: %w", err)
				return
			}

			offset += 48

			inum, inode, err := deserializedIndex(buf)
			if err != nil {
				equeue <- fmt.Errorf("failed to deserialize index (inum: %d): %w", inum, err)
				return
			}

			nqueue <- index{inum: inum, inode: inode}
		}
	}()

	// 消费者 goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for node := range nqueue {
			imap := indexs[node.inum%uint64(indexShard)]
			if imap != nil {
				imap.index[node.inum] = node.inode
			} else {
				// 这里对应着 for 循环的 len(equeue) == 0 条件
				// 防止消费者 goroutine 发生了错误已经停止了
				// 但是生产者 goroutine 还在读取反序列化索引
				// 导致不能立即执行 defer wg.Done() 做无意义的工作
				// 目的是为尽快恢复阻塞的 wg.Wait() 执行主 goroutine 返回
				equeue <- errors.New("no corresponding index shard")
				return
			}
		}
	}()

	// 等待所有 goroutine 完成
	wg.Wait()

	// 判断错误通道是否有值，捕获错误
	select {
	case err := <-equeue:
		close(equeue)
		return err
	default:
		close(equeue)
		return nil
	}
}

// crashRecoveryAllIndex 会对 regions 文件集合进行解析恢复内存索引，步骤如下：
// 1. 崩溃恢复逻辑，扫描所有的数据文件
// 2. 读取每条数据记录的前 26 字节 MetaInfo
// 3. 对这些记录进行重放，并且查看 DEL 值是否为 1
// 4. 如果是 1 则对内存的索引进行删除
// 5. 否则直接将磁盘元数据重构建为索引
// | DEL 1 | KIND 1 | EAT 8 | CAT 8 | KLEN 4 | VLEN 4 | KEY ? | VALUE ? | CRC32 4 |
func crashRecoveryAllIndex(regions map[uint64]*os.File, indexs []*indexMap) error {
	var regionIds []uint64
	for v := range regions {
		regionIds = append(regionIds, v)
	}

	// 对 regionIds 切片从小到大排序
	sort.Slice(regionIds, func(i, j int) bool {
		return regionIds[i] < regionIds[j]
	})

	// 3. 遍历每个数据文件（region）
	for _, regionId := range regionIds {
		fd, ok := regions[uint64(regionId)]
		if !ok {
			return fmt.Errorf("data file does not exist regions id: %d", regionId)
		}

		finfo, err := fd.Stat()
		if err != nil {
			return err
		}

		offset := uint64(len(dataFileMetadata))

		for offset < uint64(finfo.Size()) {
			inum, segment, err := readSegment(fd, offset, 26)
			if err != nil {
				return fmt.Errorf("failed to parse data file segment: %w", err)
			}

			imap := indexs[inum%uint64(indexShard)]
			if imap != nil {
				// 如果是一条删除操作的记录，就将该记录对应索引删除
				if segment.IsTombstone() {
					delete(imap.index, inum)
					continue
				}

				// 否则继续往下执行，构建重新 inode 索引
				imap.index[inum] = &INode{
					RegionID:  regionId,
					Position:  offset,
					Length:    segment.Size(),
					CreatedAt: segment.CreatedAt,
					ExpiredAt: segment.ExpiredAt,
				}

				offset += uint64(segment.Size())
			} else {
				// 找不到索引就抛出异常
				return errors.New("no corresponding index shard")
			}
		}

	}

	return nil
}

func validateFileHeader(file *os.File) error {
	var fileHeader [4]byte
	n, err := file.Read(fileHeader[:])
	if err != nil {
		return err
	}

	if n != len(dataFileMetadata) {
		return errors.New("file is too short to contain valid signature")
	}

	if !bytes.Equal(fileHeader[:], dataFileMetadata[:]) {
		return fmt.Errorf("unsupported data file version: %v", file.Name())
	}

	return nil
}

func checkFileSystem(path string) error {
	if !utils.IsExist(path) {
		err := os.MkdirAll(path, fsPerm)
		if err != nil {
			return err
		}
	}

	files, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	if len(files) > 0 {
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), fileExtension) {
				if strings.HasPrefix(file.Name(), "0") {
					file, err := os.Open(filepath.Join(path, file.Name()))
					if err != nil {
						return fmt.Errorf("failed to check data file: %w", err)
					}
					defer utils.CloseFile(file)

					err = validateFileHeader(file)
					if err != nil {
						return fmt.Errorf("failed to validated data file header: %w", err)
					}
				}
			}

			if !file.IsDir() && file.Name() == indexFileName {
				file, err := os.Open(filepath.Join(path, file.Name()))
				if err != nil {
					return fmt.Errorf("failed to check index file: %w", err)
				}
				defer utils.CloseFile(file)

				err = validateFileHeader(file)
				if err != nil {
					return fmt.Errorf("failed to validated index file header: %w", err)
				}
			}
		}
	}

	return nil
}

// | DEL 1 | KIND 1 | EAT 8 | CAT 8 | KLEN 4 | VLEN 4 | KEY ? | VALUE ? | CRC32 4 |
func readSegment(fd *os.File, offset uint64, bufsize int64) (uint64, *Segment, error) {
	buf := make([]byte, bufsize)

	_, err := fd.ReadAt(buf, int64(offset))
	if err != nil {
		return 0, nil, err
	}

	var seg Segment
	readOffset := 0

	// 解析 Tombstone (1 字节)
	seg.Tombstone = int8(buf[readOffset])
	readOffset++

	// 解析 Type (1 字节)
	seg.Type = Kind(buf[readOffset])
	readOffset++

	// 解析 ExpiredAt (8 字节)
	seg.ExpiredAt = binary.LittleEndian.Uint64(buf[readOffset : readOffset+8])
	readOffset += 8

	// 解析 CreatedAt (8 字节)
	seg.CreatedAt = binary.LittleEndian.Uint64(buf[readOffset : readOffset+8])
	readOffset += 8

	// 解析 KeySize (4 字节)
	seg.KeySize = binary.LittleEndian.Uint32(buf[readOffset : readOffset+4])
	readOffset += 4

	// 解析 ValueSize (4 字节)
	seg.ValueSize = binary.LittleEndian.Uint32(buf[readOffset : readOffset+4])
	readOffset += 4

	// 26 到此结束

	// 读取 Key 数据
	keybuf := make([]byte, seg.KeySize)
	_, err = fd.ReadAt(keybuf, int64(offset)+int64(readOffset))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to parse key in segment: %w", err)
	}
	readOffset += int(seg.KeySize)

	// 读取 Value 数据
	valuebuf := make([]byte, seg.ValueSize)
	_, err = fd.ReadAt(valuebuf, int64(offset)+int64(readOffset))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to parse value in segment: %w", err)
	}
	readOffset += int(seg.ValueSize)

	// 读取 checksum (4 字节)
	checksumBuf := make([]byte, 4)
	_, err = fd.ReadAt(checksumBuf, int64(offset)+int64(readOffset))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to read checksum in segment: %w", err)
	}

	// 校验 checksum
	checksum := binary.LittleEndian.Uint32(checksumBuf)

	buf = append(buf, keybuf...)
	buf = append(buf, valuebuf...)

	if checksum != crc32.ChecksumIEEE(buf) {
		return 0, nil, fmt.Errorf("failed to crc32 checksum mismatch: %d", checksum)
	}

	// 更新 Segment 数据字段为读取的 valuebuf 并且通过 Transformer 处理之后才能使用
	decodedData, err := transformer.Decode(valuebuf)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to transformer decode value in segment: %w", err)
	}

	seg.Key = keybuf
	seg.Value = decodedData

	return InodeNum(string(keybuf)), &seg, nil
}

func generateFileName(regionID uint64) (string, error) {
	fileName := formatDataFileName(regionID)
	// 验证 regionID 是否以 0 开头（仅对 8 位数有效）
	if strings.HasPrefix(fileName, "0") {
		return fileName, nil
	}
	// 超出目前的设置数据文件个数范围就抛出异常
	return "", fmt.Errorf("new region id %d cannot be converted to a valid file name", regionID)
}

// parseDataFileName 将文件名（如 0000001.wdb）中的数字部分转换为 uint64
func parseDataFileName(fileName string) (uint64, error) {
	parts := strings.Split(fileName, ".")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid file name format: %s", fileName)
	}

	// 转换为 uint16
	number, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse number from file name: %w", err)
	}

	return uint64(number), nil
}

// formatDataFileName 将 uint16 转换为文件名格式（如 1 转为 0000001.wdb）
func formatDataFileName(number uint64) string {
	return fmt.Sprintf("%08d%s", number, fileExtension)
}

// serializedIndex 将索引进行序列化为可以恢复的文件快照记录格式：
// | INUM 8 | RID 8  | POS 8 | LEN 4 | EAT 8 | CAT 8 | CRC32 4 |
func serializedIndex(inum uint64, inode *INode) ([]byte, error) {
	// 创建一个字节缓冲区
	buf := new(bytes.Buffer)

	// 按顺序写入各个字段
	binary.Write(buf, binary.LittleEndian, inum)
	binary.Write(buf, binary.LittleEndian, inode.RegionID)
	binary.Write(buf, binary.LittleEndian, inode.Position)
	binary.Write(buf, binary.LittleEndian, inode.Length)
	binary.Write(buf, binary.LittleEndian, inode.ExpiredAt)
	binary.Write(buf, binary.LittleEndian, inode.CreatedAt)

	// 计算 CRC32 校验码
	checksum := crc32.ChecksumIEEE(buf.Bytes())

	// 将 CRC32 校验码写入字节缓冲区（4 字节）
	binary.Write(buf, binary.LittleEndian, checksum)

	// 返回包含 CRC32 校验码的字节切片
	return buf.Bytes(), nil
}

// deserializedIndex 将索引文件快照恢复为内存结构体：
// | INUM 8 | RID 8  | OFS 8 | LEN 4 | EAT 8 | CAT 8 | CRC32 4 |
func deserializedIndex(data []byte) (uint64, *INode, error) {
	buf := bytes.NewReader(data)
	// 反序列化 inum
	var inum uint64
	err := binary.Read(buf, binary.LittleEndian, &inum)
	if err != nil {
		return 0, nil, err
	}

	// 反序列化 INode 的各个字段
	var inode INode
	err = binary.Read(buf, binary.LittleEndian, &inode.RegionID)
	if err != nil {
		return 0, nil, err
	}

	err = binary.Read(buf, binary.LittleEndian, &inode.Position)
	if err != nil {
		return 0, nil, err
	}

	err = binary.Read(buf, binary.LittleEndian, &inode.Length)
	if err != nil {
		return 0, nil, err
	}

	err = binary.Read(buf, binary.LittleEndian, &inode.ExpiredAt)
	if err != nil {
		return 0, nil, err
	}

	err = binary.Read(buf, binary.LittleEndian, &inode.CreatedAt)
	if err != nil {
		return 0, nil, err
	}

	// 反序列化 CRC32 校验码并验证
	var checksum uint32
	err = binary.Read(buf, binary.LittleEndian, &checksum)
	if err != nil {
		return 0, nil, err
	}

	// 计算数据的 CRC32 校验码，如果校验码不一致，返回错误
	if checksum != crc32.ChecksumIEEE(data[:len(data)-4]) {
		return 0, nil, fmt.Errorf("failed to crc32 checksum mismatch: %d", checksum)
	}

	return inum, &inode, nil
}

func serializedSegment(seg *Segment) ([]byte, error) {
	// 创建一个字节缓冲区
	buf := new(bytes.Buffer)

	// 序列化 Segment 字段
	err := binary.Write(buf, binary.LittleEndian, seg.Tombstone)
	if err != nil {
		return nil, fmt.Errorf("failed to write Tombstone: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.Type)
	if err != nil {
		return nil, fmt.Errorf("failed to write Type: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.ExpiredAt)
	if err != nil {
		return nil, fmt.Errorf("failed to write ExpiredAt: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to write CreatedAt: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.KeySize)
	if err != nil {
		return nil, fmt.Errorf("failed to write KeySize: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.ValueSize)
	if err != nil {
		return nil, fmt.Errorf("failed to write ValueSize: %w", err)
	}

	// 序列化 Key 和 Value 数据
	err = binary.Write(buf, binary.LittleEndian, seg.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to write Key: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to write Value: %w", err)
	}

	// 计算 CRC32 校验码
	checksum := crc32.ChecksumIEEE(buf.Bytes())

	// 将 CRC32 校验码写入字节缓冲区（4 字节）
	err = binary.Write(buf, binary.LittleEndian, checksum)
	if err != nil {
		return nil, fmt.Errorf("failed to write checksum: %w", err)
	}

	// 返回包含 CRC32 校验码的字节切片
	return buf.Bytes(), nil
}

// 垃圾回收压缩器工作原理
// 1. 如果磁盘上没有索引快照，就全局扫描恢复索引
// 2. 当索引恢复之后，运行了一段时间出发垃圾回收
// 3. 启动 GC 扫描磁盘数据文件和内存索引最新版比较
// 4. 如果磁盘上的文件记录和索引记录对上就迁移到新文件
// 5. 如果没有对上说明是旧文件，不要管它重复这个过程
// 6. 直到 GC 扫描完整个数据文件完成，最后删除这个文件
// 7. PS：重点是反向扫描，通过磁盘数据文件中 Key 名找内存到记录比较
// 8. 如果通过内存索引来找，会出现无法确定一个文件是否扫描干净
// 9. 因为内存索引的对应的数据记录会分配在不同数据文件中
func (lfs *LogStructuredFS) compressDirtyRegion() error {
	// 执行 gc 垃圾回收逻辑
	if len(lfs.regions) >= 3 {
		var regionIds []uint64
		for v := range lfs.regions {
			regionIds = append(regionIds, v)
		}
		// 对 regionIds 切片从小到大排序
		sort.Slice(regionIds, func(i, j int) bool {
			return regionIds[i] < regionIds[j]
		})
		// 找到前两个旧数据文件
		for i := 0; i < len(regionIds)-1; i++ {
			lfs.dirtyRegion = append(lfs.dirtyRegion, lfs.regions[regionIds[i]])
		}
		// 执行对旧数据文件的压缩
		for _, fd := range lfs.dirtyRegion {
			finfo, err := fd.Stat()
			if err != nil {
				return err
			}

			readOffset := uint64(len(dataFileMetadata))

			for readOffset < uint64(finfo.Size()) {
				inum, segment, err := readSegment(fd, uint64(readOffset), 26)
				if err != nil {
					return err
				}
				imap := lfs.indexs[inum%uint64(indexShard)]
				if imap.index[inum].CreatedAt == segment.CreatedAt {
					// 迁移数据到新的数据文件中
					appendBinaryToFile(lfs.active, segment)
					// 把 inum 和新的 region 映射起来
					imap.index[inum].RegionID = lfs.regionID
					imap.index[inum].Position = lfs.offset + uint64(segment.Size())
					readOffset += uint64(segment.Size())
				} else {
					continue
				}
			}

			err = lfs.active.Sync()
			if err != nil {
				return fmt.Errorf("failed to close active migrate region: %w", err)
			}
			// 删除这个文件
		}
	} else {
		clog.Warnf("dirty region (%d) does not meet garbage collection status", len(lfs.regions))
	}

	return nil
}

// 开始序列化小端数据，ToLittleEndian(lfs.active,seg)，需要对 seg 进行压缩处理再写入
func appendBinaryToFile(_ *os.File, _ *Segment) error {
	return nil
}
