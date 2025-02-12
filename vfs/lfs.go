package vfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
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
	"github.com/spaolacci/murmur3"
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
	SEGMENT_PADDING = 26
)

var (
	indexShard       = 5
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
	Threshold uint8
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
	mu    sync.RWMutex
	index map[uint64]*INode
}

// LogStructuredFS represents the virtual file storage system.
type LogStructuredFS struct {
	mu          sync.RWMutex
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

// PutSegment inserts a Segment record into the LogStructuredFS virtual file system.
func (lfs *LogStructuredFS) PutSegment(key string, seg *Segment) error {
	inum := InodeNum(key)
	// Append data to the active region with a lock.
	err := appendDataWithLock(&lfs.mu, lfs.active, seg)
	if err != nil {
		return err
	}

	// Update the inode metadata within a critical section.
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

	if lfs.offset >= uint64(regionThreshold) {
		lfs.mu.Lock()
		err := lfs.createActiveRegion()
		lfs.mu.Unlock()
		if err != nil {
			return err
		}
	}

	// Use a read lock to prevent conflicts during index updates.
	lfs.mu.RLock()
	defer lfs.mu.RUnlock()

	// Select an index shard based on the hash function and update it.
	// To avoid locking the entire index, only the relevant shard is locked.
	imap := lfs.indexs[inum%uint64(indexShard)]
	imap.mu.Lock()
	imap.index[inum] = inode
	imap.mu.Unlock()

	return nil
}

func (lfs *LogStructuredFS) BatchFetchSegments(keys ...string) ([]*Segment, error) {
	var segs []*Segment
	for _, key := range keys {
		seg, err := lfs.FetchSegment(key)
		if err != nil {
			return nil, err
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

func (lfs *LogStructuredFS) DeleteSegment(key string) error {
	inum := InodeNum(key)
	// Calculate the index shard
	imap := lfs.indexs[inum%uint64(indexShard)]
	if imap == nil {
		return fmt.Errorf("inode index shard for %d not found", inum)
	}

	// Check if the inode is expired
	imap.mu.Lock()
	delete(imap.index, inum)
	imap.mu.Unlock()

	seg := NewTombstoneSegment([]byte(key))
	err := appendDataWithLock(&lfs.mu, lfs.active, seg)
	if err != nil {
		return err
	}

	lfs.mu.Lock()
	lfs.offset += uint64(seg.Size())
	lfs.mu.Unlock()

	return nil
}

func (lfs *LogStructuredFS) FetchSegment(key string) (*Segment, error) {
	inum := InodeNum(key)
	// Calculate the index shard
	imap := lfs.indexs[inum%uint64(indexShard)]
	if imap == nil {
		return nil, fmt.Errorf("inode index shard for %d not found", inum)
	}

	// Retrieve inode info
	imap.mu.RLock()
	inode, ok := imap.index[inum]
	imap.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("inode index for %d not found", inum)
	}

	// Check if the inode is expired
	if inode.ExpiredAt <= uint64(time.Now().Unix()) && inode.ExpiredAt != 0 {
		imap.mu.Lock()
		delete(imap.index, inum)
		imap.mu.Unlock()
		return nil, fmt.Errorf("inode index for %d has expired", inum)
	}

	// Retrieve the corresponding data region
	lfs.mu.RLock()
	fd, ok := lfs.regions[inode.RegionID]
	lfs.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("data region with ID %d not found", inode.RegionID)
	}

	// Read the segment from the data region
	lfs.mu.RLock()
	defer lfs.mu.RUnlock()
	_, segment, err := readSegment(fd, inode.Position, SEGMENT_PADDING)
	if err != nil {
		return nil, fmt.Errorf("failed to read segment: %w", err)
	}

	// Return the fetched segment
	return segment, nil
}

func (lfs *LogStructuredFS) KeysCount() int {
	keys := 0
	for _, imap := range lfs.indexs {
		imap.mu.RLock()
		keys += len(imap.index)
		imap.mu.RUnlock()
	}
	return keys
}

func InodeNum(key string) uint64 {
	return murmur3.Sum64([]byte(key))
}

func (lfs *LogStructuredFS) changeRegions() error {
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
	lfs.regions[lfs.regionID] = lfs.active

	return nil
}

func (lfs *LogStructuredFS) recoverRegions() error {
	// Single-thread recovery does not require locking
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
					return fmt.Errorf("failed to get region id: %w", err)
				}
				lfs.regions[regionID] = regions
			}
		}
	}

	// Only find the largest file if there are more than one data files
	if len(lfs.regions) >= 1 {
		var regionIds []uint64
		for v := range lfs.regions {
			regionIds = append(regionIds, v)
		}
		// Sort the regionIds slice in ascending order
		sort.Slice(regionIds, func(i, j int) bool {
			return regionIds[i] < regionIds[j]
		})

		// Find the latest version of the data file
		lfs.regionID = regionIds[len(regionIds)-1]

		// Create a new file if the largest region file exceeds the threshold, otherwise, no need to create a new file
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
		// If it is an empty directory, create a writable data file
		return lfs.createActiveRegion()
	}

	return nil
}

// recoveryIndex performs index recovery operations on data files stored on disk.
// Steps:
//  1. Read the index snapshot file to restore the index.
//  2. Unlike bitcask, where hint files are generated during the compressor process,
//     in bitcask, hint files are created during compression but do not represent
//     the full state of the in-memory index.
//  3. wiredkv adopts a completely different design. If the system was closed normally,
//     an index file is generated upon closure.
//  4. If the data file has an associated index file, the index is restored directly
//     from the index file.
//  5. If no index file exists, a global scan of the data files is performed at startup
//     to reconstruct the index file.
func (lfs *LogStructuredFS) recoveryIndex() error {
	// Construct the full file path
	filePath := filepath.Join(lfs.directory, indexFileName)
	if utils.IsExist(filePath) {
		// If the index file exists, restore it
		file, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("failed to open index file: %w", err)
		}
		defer file.Close()

		err = recoveryIndex(file, lfs.indexs)
		if err != nil {
			return fmt.Errorf("failed to recover index mapping: %w", err)
		}

		return nil
	}

	// If the index file does not exist, recover by globally scanning the regions files
	// If the data files are very large and numerous, recovery time increases significantly.
	// Frequent garbage collection reduces the size of data files and speeds up startup time.
	// However, frequent garbage collection may negatively impact overall read/write performance.
	return crashRecoveryAllIndex(lfs.regions, lfs.indexs)
}

func (lfs *LogStructuredFS) SetCompressor(compressor Compressor) {
	transformer.SetCompressor(compressor)
}

func (lfs *LogStructuredFS) SetEncryptor(encryptor Encryptor, secret []byte) error {
	return transformer.SetEncryptor(encryptor, secret)
}

func (lfs *LogStructuredFS) StartRegionGC(cycle_second time.Duration) {
	// Return if the garbage collector is not in the initial state.
	if lfs.gcstate != GC_INIT {
		return
	}

	// Create a ticker that triggers at the specified interval.
	ticker := time.NewTicker(cycle_second)
	// Channel to control the graceful exit of the garbage collection goroutine.
	lfs.gcdone = make(chan struct{}, 1)

	// Start a goroutine to continuously receive messages from the ticker channel.
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Skip this cycle if the previous garbage collection is still running.
				if lfs.gcstate == GC_RUNNING {
					continue
				}

				// Execute the garbage collection logic.
				err := lfs.compressDirtyRegion()
				if err != nil {
					clog.Warnf("failed to compress dirty region: %s", err)
				}

				// Update the state to indicate garbage collection has stopped.
				lfs.gcstate = GC_STOP
			case <-lfs.gcdone:
				// If garbage collection is running, delay its exit to prevent dirty data
				// from being generated due to interrupted operations.
				for lfs.gcstate == GC_RUNNING {
					time.Sleep(3 * time.Second)
				}
				// Reset the garbage collector state to the initial state.
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
	// Single region max size = 255GB
	regionThreshold = int64(opt.Threshold) * GB

	err := checkFileSystem(opt.Path)
	if err != nil {
		return nil, err
	}

	fsPerm = opt.FsPerm
	instance := &LogStructuredFS{
		mu:        sync.RWMutex{},
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

	// First, perform recovery operations on existing data files and initialize the in-memory data version number
	err = instance.recoverRegions()
	if err != nil {
		return nil, fmt.Errorf("failed to recover data regions: %w", err)
	}

	err = instance.recoveryIndex()
	if err != nil {
		return nil, fmt.Errorf("failed to recover regions index: %w", err)
	}

	// Singleton pattern, but other packages can still create an instance with new(LogStructuredFS), which makes this ineffective
	return instance, nil
}

// Before closing, always check if GC (garbage collection) is executing.
// If GC is executing, do not close blindly.
func (lfs *LogStructuredFS) CloseFS() error {
	lfs.mu.Lock()
	defer lfs.mu.Unlock()
	for _, file := range lfs.regions {
		err := utils.FlushToDisk(file)
		if err != nil {
			return fmt.Errorf("failed to close region file: %w", err)
		}
	}

	// If there is a snapshot of the index file, recover from the snapshot.
	// Otherwise, perform a global scan.
	return lfs.ExportSnapshotIndex()
}

// ExportSnapshotIndex is the operation performed during a normal program exit.
// exporting the in-memory index snapshot to a file on disk.
// The current design has limitations for systems with low memory resources,
// such as those with RAM of 512 MB < 1 GB.
// If a 1 GB snapshot cannot be fully serialized to disk,
// mapping large files into memory may not be a good choice,
// as it consumes a significant amount of virtual memory space and may lead to
// swapping memory pages to disk.
func (lfs *LogStructuredFS) ExportSnapshotIndex() error {
	filePath := filepath.Join(lfs.directory, indexFileName)
	fd, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, fsPerm)
	if err != nil {
		return fmt.Errorf("failed to generate index snapshot file: %w", err)
	}
	defer utils.FlushToDisk(fd)

	n, err := fd.Write(dataFileMetadata)
	if err != nil {
		return fmt.Errorf("failed to write index file metadata: %w", err)
	}

	if n != len(dataFileMetadata) {
		return errors.New("index file metadata write incomplete")
	}

	for _, imap := range lfs.indexs {
		imap.mu.RLock()
		defer imap.mu.RUnlock()
		for inum, inode := range imap.index {
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
	offset := int64(len(dataFileMetadata))

	finfo, err := fd.Stat()
	if err != nil {
		return err
	}

	type index struct {
		inum  uint64
		inode *INode
	}

	nqueue := make(chan index, (finfo.Size()-offset)/48)
	equeue := make(chan error, 1)

	var wg sync.WaitGroup

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

	wg.Add(1)
	go func() {
		defer wg.Done()
		for node := range nqueue {
			imap := indexs[node.inum%uint64(indexShard)]
			if imap != nil {
				imap.index[node.inum] = node.inode
			} else {
				// This corresponds to the condition len(queue) == 0 in the for loop.
				// It prevents a situation where the consumer goroutine has encountered an error and stopped,
				// but the producer goroutine is still reading and deserializing the index.
				// As a result, it avoids delaying the execution of defer wg.Done(), which would perform meaningless work.
				// The goal is to resume the blocked wg.Wait() as quickly as possible,
				// allowing the main goroutine to return promptly.
				equeue <- errors.New("no corresponding index shard")
				return
			}
		}
	}()

	wg.Wait()

	select {
	case err := <-equeue:
		close(equeue)
		return err
	default:
		close(equeue)
		return nil
	}
}

// crashRecoveryAllIndex parses the regions file collection and restores the in-memory index with the following.
// Steps:
// 1. Crash recovery logic scans all data files.
// 2. Reads the first 26 bytes of MetaInfo from each data record.
// 3. Replays these records and checks whether the DEL value is 1.
// 4. If DEL is 1, the corresponding entry is deleted from the in-memory index.
// 5. Otherwise, the disk metadata is reconstructed into the index.
// | DEL 1 | KIND 1 | EAT 8 | CAT 8 | KLEN 4 | VLEN 4 | KEY ? | VALUE ? | CRC32 4 |
func crashRecoveryAllIndex(regions map[uint64]*os.File, indexs []*indexMap) error {
	var regionIds []uint64
	for v := range regions {
		regionIds = append(regionIds, v)
	}

	sort.Slice(regionIds, func(i, j int) bool {
		return regionIds[i] < regionIds[j]
	})

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
			inum, segment, err := readSegment(fd, offset, SEGMENT_PADDING)
			if err != nil {
				return fmt.Errorf("failed to parse data file segment: %w", err)
			}

			imap := indexs[inum%uint64(indexShard)]
			if imap != nil {
				if segment.IsTombstone() {
					delete(imap.index, inum)
					offset += uint64(segment.Size())
					continue
				}

				if segment.ExpiredAt <= uint64(time.Now().Unix()) && segment.ExpiredAt != 0 {
					offset += uint64(segment.Size())
					continue
				}

				imap.index[inum] = &INode{
					RegionID:  regionId,
					Position:  offset,
					Length:    segment.Size(),
					CreatedAt: segment.CreatedAt,
					ExpiredAt: segment.ExpiredAt,
				}

				offset += uint64(segment.Size())
			} else {
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
					defer file.Close()

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
				defer file.Close()

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

	// Parse Tombstone (1 byte)
	seg.Tombstone = int8(buf[readOffset])
	readOffset++

	// Parse Type (1 byte)
	seg.Type = Kind(buf[readOffset])
	readOffset++

	// Parse ExpiredAt (8 bytes)
	seg.ExpiredAt = binary.LittleEndian.Uint64(buf[readOffset : readOffset+8])
	readOffset += 8

	// Parse CreatedAt (8 bytes)
	seg.CreatedAt = binary.LittleEndian.Uint64(buf[readOffset : readOffset+8])
	readOffset += 8

	// Parse KeySize (4 bytes)
	seg.KeySize = binary.LittleEndian.Uint32(buf[readOffset : readOffset+4])
	readOffset += 4

	// Parse ValueSize (4 bytes)
	seg.ValueSize = binary.LittleEndian.Uint32(buf[readOffset : readOffset+4])
	readOffset += 4

	// End of Header 26 bytes

	// Read Key data
	keybuf := make([]byte, seg.KeySize)
	_, err = fd.ReadAt(keybuf, int64(offset)+int64(readOffset))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to parse key in segment: %w", err)
	}
	readOffset += int(seg.KeySize)

	// Read Value data
	valuebuf := make([]byte, seg.ValueSize)
	_, err = fd.ReadAt(valuebuf, int64(offset)+int64(readOffset))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to parse value in segment: %w", err)
	}
	readOffset += int(seg.ValueSize)

	// Read checksum (4 bytes)
	checksumBuf := make([]byte, 4)
	_, err = fd.ReadAt(checksumBuf, int64(offset)+int64(readOffset))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to read checksum in segment: %w", err)
	}

	// Verify checksum
	checksum := binary.LittleEndian.Uint32(checksumBuf)

	buf = append(buf, keybuf...)
	buf = append(buf, valuebuf...)

	if checksum != crc32.ChecksumIEEE(buf) {
		return 0, nil, fmt.Errorf("failed to crc32 checksum mismatch: %d", checksum)
	}

	// Update Segment data fields with the read valuebuf and process it through Transformer before use
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
	// Verify if regionID starts with 0 (valid only for 8 digits)
	if strings.HasPrefix(fileName, "0") {
		return fileName, nil
	}
	// Throw an exception if the regionID exceeds the current set number of data files
	return "", fmt.Errorf("new region id %d cannot be converted to a valid file name", regionID)
}

// parseDataFileName converts the numeric part of the file name (e.g., 0000001.wdb) to uint64
func parseDataFileName(fileName string) (uint64, error) {
	parts := strings.Split(fileName, ".")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid file name format: %s", fileName)
	}

	// Convert to uint64
	number, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse number from file name: %w", err)
	}

	return uint64(number), nil
}

// formatDataFileName converts uint64 to file name format (e.g., 1 to 0000001.wdb)
func formatDataFileName(number uint64) string {
	return fmt.Sprintf("%08d%s", number, fileExtension)
}

// serializedIndex serializes the index to a recoverable file snapshot record format:
// | INUM 8 | RID 8  | POS 8 | LEN 4 | EAT 8 | CAT 8 | CRC32 4 |
func serializedIndex(inum uint64, inode *INode) ([]byte, error) {
	// Create a byte buffer
	buf := new(bytes.Buffer)

	// Write each field in order
	binary.Write(buf, binary.LittleEndian, inum)
	binary.Write(buf, binary.LittleEndian, inode.RegionID)
	binary.Write(buf, binary.LittleEndian, inode.Position)
	binary.Write(buf, binary.LittleEndian, inode.Length)
	binary.Write(buf, binary.LittleEndian, inode.ExpiredAt)
	binary.Write(buf, binary.LittleEndian, inode.CreatedAt)

	// Calculate CRC32 checksum
	checksum := crc32.ChecksumIEEE(buf.Bytes())

	// Write CRC32 checksum to byte buffer (4 bytes)
	binary.Write(buf, binary.LittleEndian, checksum)

	// Return byte slice containing CRC32 checksum
	return buf.Bytes(), nil
}

// deserializedIndex restores the index file snapshot to an in-memory struct:
// | INUM 8 | RID 8  | OFS 8 | LEN 4 | EAT 8 | CAT 8 | CRC32 4 |
func deserializedIndex(data []byte) (uint64, *INode, error) {
	buf := bytes.NewReader(data)
	var inum uint64
	err := binary.Read(buf, binary.LittleEndian, &inum)
	if err != nil {
		return 0, nil, err
	}

	// Deserialize each field of INode
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

	// Deserialize and verify CRC32 checksum
	var checksum uint32
	err = binary.Read(buf, binary.LittleEndian, &checksum)
	if err != nil {
		return 0, nil, err
	}

	// Calculate CRC32 checksum of data, return an error if checksum does not match
	if checksum != crc32.ChecksumIEEE(data[:len(data)-4]) {
		return 0, nil, fmt.Errorf("failed to crc32 checksum mismatch: %d", checksum)
	}

	return inum, &inode, nil
}

func serializedSegment(seg *Segment) ([]byte, error) {
	buf := new(bytes.Buffer)

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

	err = binary.Write(buf, binary.LittleEndian, seg.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to write Key: %w", err)
	}

	err = binary.Write(buf, binary.LittleEndian, seg.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to write Value: %w", err)
	}

	checksum := crc32.ChecksumIEEE(buf.Bytes())

	err = binary.Write(buf, binary.LittleEndian, checksum)
	if err != nil {
		return nil, fmt.Errorf("failed to write checksum: %w", err)
	}

	return buf.Bytes(), nil
}

// Garbage Collection Compressor
// Steps:
// 1. If no index snapshot exists on disk, perform a global scan to restore the index.
// 2. After the index is restored, run for a while before triggering garbage collection.
// 3. Start the GC process by scanning disk data files and comparing them with the latest in-memory index.
// 4. If a record in the disk file matches the index record, migrate it to a new file.
// 5. If no match is found, the file is considered outdated; skip it and continue the process.
// 6. Repeat the process until the GC has scanned all data files, then delete the original files.
// 7. Note: The key point is reverse scanning. Use keys from the disk data files to locate and compare records in memory.
// 8. If the in-memory index is used to locate records, it becomes impossible to determine if a file has been fully scanned.
// 9. This is because records in the in-memory index may be distributed across multiple data files on disk.
func (lfs *LogStructuredFS) compressDirtyRegion() error {
	if len(lfs.regions) >= 5 {
		var regionIds []uint64
		for v := range lfs.regions {
			regionIds = append(regionIds, v)
		}
		sort.Slice(regionIds, func(i, j int) bool {
			return regionIds[i] < regionIds[j]
		})

		// find 40% dirty region
		for i := 0; i < 4 && i < len(regionIds); i++ {
			lfs.dirtyRegion = append(lfs.dirtyRegion, lfs.regions[regionIds[i]])
		}

		for _, fd := range lfs.dirtyRegion {
			finfo, err := fd.Stat()
			if err != nil {
				return err
			}

			readOffset := uint64(len(dataFileMetadata))

			for readOffset < uint64(finfo.Size()) {
				inum, segment, err := readSegment(fd, uint64(readOffset), SEGMENT_PADDING)
				if err != nil {
					return err
				}

				imap := lfs.indexs[inum%uint64(indexShard)]
				if imap != nil {
					imap.mu.RLock()
					inode, ok := imap.index[inum]
					imap.mu.RUnlock()

					if !ok {
						continue
					}

					if isValid(segment, inode) {
						err := appendDataWithLock(&lfs.mu, lfs.active, segment)
						if err != nil {
							return err
						}

						lfs.mu.Lock()
						delete(lfs.regions, inode.RegionID)
						lfs.mu.Unlock()

						imap.mu.Lock()
						inode.Position = lfs.offset
						inode.RegionID = lfs.regionID
						imap.mu.Unlock()

						lfs.mu.Lock()
						lfs.offset += uint64(segment.Size())
						lfs.mu.Unlock()

						readOffset += uint64(segment.Size())
					} else {
						// next segment
						continue
					}

				} else {
					return fmt.Errorf("imap is nil for inum = %d", inum)
				}

				if lfs.offset >= uint64(regionThreshold) {
					err := lfs.changeRegions()
					if err != nil {
						return fmt.Errorf("failed to close active migrate region: %w", err)
					}
				}

				// Delete dirty region file
				lfs.mu.Lock()
				err = os.Remove(filepath.Join(lfs.directory, fd.Name()))
				lfs.mu.Unlock()
				if err != nil {
					return fmt.Errorf("failed to remove dirty region: %w", err)
				}
			}

		}
		// Cleanup dirty region
		lfs.dirtyRegion = nil
	} else {
		clog.Warnf("dirty region (%d%%) does not meet garbage collection status", len(lfs.regions)/10)
	}

	return nil
}

func isValid(seg *Segment, inode *INode) bool {
	return !seg.IsTombstone() &&
		seg.CreatedAt == inode.CreatedAt &&
		uint64(time.Now().Unix()) < seg.ExpiredAt
}

// Start serializing little-endian data, needs to compress seg before writing.
func appendDataWithLock(mux *sync.RWMutex, fd *os.File, seg *Segment) error {
	bytes, err := serializedSegment(seg)
	if err != nil {
		return err
	}

	mux.Lock()
	defer mux.Unlock()

	// Write the byte stream to the file
	n, err := fd.Write(bytes)
	if err != nil {
		return fmt.Errorf("failed to append binary data to active region: %w", err)
	}
	// Check if the number of written bytes matches
	if n != len(bytes) {
		return fmt.Errorf("partial write error: expected %d bytes, but wrote %d bytes", len(bytes), n)
	}
	return nil
}
