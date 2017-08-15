/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"encoding/hex"
	"errors"
	"hash/crc32"
	"path"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// Returns number of errors that occurred the most (incl. nil) and the
// corresponding error value. NB When there is more than one error value that
// occurs maximum number of times, the error value returned depends on how
// golang's map orders keys. This doesn't affect correctness as long as quorum
// value is greater than or equal to simple majority, since none of the equally
// maximal values would occur quorum or more number of times.
func reduceErrs(errs []error, ignoredErrs []error) (maxCount int, maxErr error) {
	errorCounts := make(map[error]int)
	errs = errorsCause(errs)
	for _, err := range errs {
		if isErrIgnored(err, ignoredErrs...) {
			continue
		}
		errorCounts[err]++
	}

	max := 0
	for err, count := range errorCounts {
		switch {
		case max < count:
			max = count
			maxErr = err

		// Prefer `nil` over other error values with the same
		// number of occurrences.
		case max == count && err == nil:
			maxErr = err
		}
	}
	return max, maxErr
}

// reduceQuorumErrs behaves like reduceErrs by only for returning
// values of maximally occurring errors validated against a generic
// quorum number that can be read or write quorum depending on usage.
// Additionally a special error is provided to be returned in case
// quorum is not satisfied.
func reduceQuorumErrs(errs []error, ignoredErrs []error, quorum int, quorumErr error) (maxErr error) {
	maxCount, maxErr := reduceErrs(errs, ignoredErrs)
	if maxErr == nil && maxCount >= quorum {
		// Success in quorum.
		return nil
	}
	if maxErr != nil && maxCount >= quorum {
		// Errors in quorum.
		return traceError(maxErr, errs...)
	}
	// No quorum satisfied.
	maxErr = traceError(quorumErr, errs...)
	return
}

// reduceReadQuorumErrs behaves like reduceErrs but only for returning
// values of maximally occurring errors validated against readQuorum.
func reduceReadQuorumErrs(errs []error, ignoredErrs []error, readQuorum int) (maxErr error) {
	return reduceQuorumErrs(errs, ignoredErrs, readQuorum, errXLReadQuorum)
}

// reduceWriteQuorumErrs behaves like reduceErrs but only for returning
// values of maximally occurring errors validated against writeQuorum.
func reduceWriteQuorumErrs(errs []error, ignoredErrs []error, writeQuorum int) (maxErr error) {
	return reduceQuorumErrs(errs, ignoredErrs, writeQuorum, errXLWriteQuorum)
}

// Similar to 'len(slice)' but returns the actual elements count
// skipping the unallocated elements.
func diskCount(disks []StorageAPI) int {
	diskCount := 0
	for _, disk := range disks {
		if disk == nil {
			continue
		}
		diskCount++
	}
	return diskCount
}

// hashOrder - hashes input key to return consistent
// hashed integer slice. Returned integer order is salted
// with an input key. This results in consistent order.
// NOTE: collisions are fine, we are not looking for uniqueness
// in the slices returned.
func hashOrder(key string, cardinality int) []int {
	if cardinality < 0 {
		// Returns an empty int slice for negative cardinality.
		return nil
	}
	nums := make([]int, cardinality)
	keyCrc := crc32.Checksum([]byte(key), crc32.IEEETable)

	start := int(uint32(keyCrc)%uint32(cardinality)) | 1
	for i := 1; i <= cardinality; i++ {
		nums[i-1] = 1 + ((start + i) % cardinality)
	}
	return nums
}

func parseXLStat(xlMetaBuf []byte) (si statInfo, e error) {
	// obtain stat info.
	stat := statInfo{}
	// fetching modTime.
	modTime, err := time.Parse(time.RFC3339, gjson.GetBytes(xlMetaBuf, "stat.modTime").String())
	if err != nil {
		return si, err
	}
	stat.ModTime = modTime
	// obtain Stat.Size .
	stat.Size = gjson.GetBytes(xlMetaBuf, "stat.size").Int()
	return stat, nil
}

func parseXLVersion(xlMetaBuf []byte) string {
	return gjson.GetBytes(xlMetaBuf, "version").String()
}

func parseXLFormat(xlMetaBuf []byte) string {
	return gjson.GetBytes(xlMetaBuf, "format").String()
}

func parseXLRelease(xlMetaBuf []byte) string {
	return gjson.GetBytes(xlMetaBuf, "minio.release").String()
}

func parseXLErasureInfo(xlMetaBuf []byte) (ErasureInfo, error) {
	erasure := ErasureInfo{}
	erasureResult := gjson.GetBytes(xlMetaBuf, "erasure")
	// parse the xlV1Meta.Erasure.Distribution.
	disResult := erasureResult.Get("distribution").Array()

	distribution := make([]int, len(disResult))
	for i, dis := range disResult {
		distribution[i] = int(dis.Int())
	}
	erasure.Distribution = distribution

	erasure.Algorithm = erasureResult.Get("algorithm").String()
	erasure.DataBlocks = int(erasureResult.Get("data").Int())
	erasure.ParityBlocks = int(erasureResult.Get("parity").Int())
	erasure.BlockSize = erasureResult.Get("blockSize").Int()
	erasure.Index = int(erasureResult.Get("index").Int())

	checkSumsResult := erasureResult.Get("checksum").Array()
	// Parse xlMetaV1.Erasure.Checksum array.
	checkSums := make([]ChecksumInfo, len(checkSumsResult))
	for i, v := range checkSumsResult {
		algorithm := BitrotAlgorithmFromString(v.Get("algorithm").String())
		if !algorithm.Available() {
			return erasure, traceError(errBitrotHashAlgoInvalid)
		}
		hash, err := hex.DecodeString(v.Get("hash").String())
		if err != nil {
			return erasure, traceError(err)
		}
		checkSums[i] = ChecksumInfo{Name: v.Get("name").String(), Algorithm: algorithm, Hash: hash}
	}
	erasure.Checksums = checkSums
	return erasure, nil
}

func parseXLParts(xlMetaBuf []byte) []objectPartInfo {
	// Parse the XL Parts.
	partsResult := gjson.GetBytes(xlMetaBuf, "parts").Array()
	partInfo := make([]objectPartInfo, len(partsResult))
	for i, p := range partsResult {
		info := objectPartInfo{}
		info.Number = int(p.Get("number").Int())
		info.Name = p.Get("name").String()
		info.ETag = p.Get("etag").String()
		info.Size = p.Get("size").Int()
		partInfo[i] = info
	}
	return partInfo
}

func parseXLMetaMap(xlMetaBuf []byte) map[string]string {
	// Get xlMetaV1.Meta map.
	metaMapResult := gjson.GetBytes(xlMetaBuf, "meta").Map()
	metaMap := make(map[string]string)
	for key, valResult := range metaMapResult {
		metaMap[key] = valResult.String()
	}
	return metaMap
}

// Constructs XLMetaV1 using `gjson` lib to retrieve each field.
func xlMetaV1UnmarshalJSON(xlMetaBuf []byte) (xlMeta xlMetaV1, e error) {
	// obtain version.
	xlMeta.Version = parseXLVersion(xlMetaBuf)
	// obtain format.
	xlMeta.Format = parseXLFormat(xlMetaBuf)
	// Parse xlMetaV1.Stat .
	stat, err := parseXLStat(xlMetaBuf)
	if err != nil {
		return xlMeta, err
	}

	xlMeta.Stat = stat
	// parse the xlV1Meta.Erasure fields.
	xlMeta.Erasure, err = parseXLErasureInfo(xlMetaBuf)
	if err != nil {
		return xlMeta, err
	}

	// Parse the XL Parts.
	xlMeta.Parts = parseXLParts(xlMetaBuf)
	// Get the xlMetaV1.Realse field.
	xlMeta.Minio.Release = parseXLRelease(xlMetaBuf)
	// parse xlMetaV1.
	xlMeta.Meta = parseXLMetaMap(xlMetaBuf)

	return xlMeta, nil
}

// read xl.json from the given disk, parse and return xlV1MetaV1.Parts.
func readXLMetaParts(disk StorageAPI, bucket string, object string) ([]objectPartInfo, error) {
	// Reads entire `xl.json`.
	xlMetaBuf, err := disk.ReadAll(bucket, path.Join(object, xlMetaJSONFile))
	if err != nil {
		return nil, traceError(err)
	}
	// obtain xlMetaV1{}.Partsusing `github.com/tidwall/gjson`.
	xlMetaParts := parseXLParts(xlMetaBuf)

	return xlMetaParts, nil
}

// read xl.json from the given disk and parse xlV1Meta.Stat and xlV1Meta.Meta using gjson.
func readXLMetaStat(disk StorageAPI, bucket string, object string) (si statInfo, mp map[string]string, e error) {
	// Reads entire `xl.json`.
	xlMetaBuf, err := disk.ReadAll(bucket, path.Join(object, xlMetaJSONFile))
	if err != nil {
		return si, nil, traceError(err)
	}

	// obtain version.
	xlVersion := parseXLVersion(xlMetaBuf)

	// obtain format.
	xlFormat := parseXLFormat(xlMetaBuf)

	// Validate if the xl.json we read is sane, return corrupted format.
	if !isXLMetaValid(xlVersion, xlFormat) {
		// For version mismatchs and unrecognized format, return corrupted format.
		return si, nil, traceError(errCorruptedFormat)
	}

	// obtain xlMetaV1{}.Meta using `github.com/tidwall/gjson`.
	xlMetaMap := parseXLMetaMap(xlMetaBuf)

	// obtain xlMetaV1{}.Stat using `github.com/tidwall/gjson`.
	xlStat, err := parseXLStat(xlMetaBuf)
	if err != nil {
		return si, nil, traceError(err)
	}

	// Return structured `xl.json`.
	return xlStat, xlMetaMap, nil
}

// readXLMeta reads `xl.json` and returns back XL metadata structure.
func readXLMeta(disk StorageAPI, bucket string, object string) (xlMeta xlMetaV1, err error) {
	// Reads entire `xl.json`.
	xlMetaBuf, err := disk.ReadAll(bucket, path.Join(object, xlMetaJSONFile))
	if err != nil {
		return xlMetaV1{}, traceError(err)
	}
	// obtain xlMetaV1{} using `github.com/tidwall/gjson`.
	xlMeta, err = xlMetaV1UnmarshalJSON(xlMetaBuf)
	if err != nil {
		return xlMetaV1{}, traceError(err)
	}
	// Return structured `xl.json`.
	return xlMeta, nil
}

// Reads all `xl.json` metadata as a xlMetaV1 slice.
// Returns error slice indicating the failed metadata reads.
func readAllXLMetadata(disks []StorageAPI, bucket, object string) ([]xlMetaV1, []error) {
	errs := make([]error, len(disks))
	metadataArray := make([]xlMetaV1, len(disks))
	var wg = &sync.WaitGroup{}
	// Read `xl.json` parallelly across disks.
	for index, disk := range disks {
		if disk == nil {
			errs[index] = errDiskNotFound
			continue
		}
		wg.Add(1)
		// Read `xl.json` in routine.
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			var err error
			metadataArray[index], err = readXLMeta(disk, bucket, object)
			if err != nil {
				errs[index] = err
				return
			}
		}(index, disk)
	}

	// Wait for all the routines to finish.
	wg.Wait()

	// Return all the metadata.
	return metadataArray, errs
}

// Return shuffled partsMetadata depending on distribution.
func shufflePartsMetadata(partsMetadata []xlMetaV1, distribution []int) (shuffledPartsMetadata []xlMetaV1) {
	if distribution == nil {
		return partsMetadata
	}
	shuffledPartsMetadata = make([]xlMetaV1, len(partsMetadata))
	// Shuffle slice xl metadata for expected distribution.
	for index := range partsMetadata {
		blockIndex := distribution[index]
		shuffledPartsMetadata[blockIndex-1] = partsMetadata[index]
	}
	return shuffledPartsMetadata
}

// shuffleDisks - shuffle input disks slice depending on the
// erasure distribution. Return shuffled slice of disks with
// their expected distribution.
func shuffleDisks(disks []StorageAPI, distribution []int) (shuffledDisks []StorageAPI) {
	if distribution == nil {
		return disks
	}
	shuffledDisks = make([]StorageAPI, len(disks))
	// Shuffle disks for expected distribution.
	for index := range disks {
		blockIndex := distribution[index]
		shuffledDisks[blockIndex-1] = disks[index]
	}
	return shuffledDisks
}

// evalDisks - returns a new slice of disks where nil is set if
// the corresponding error in errs slice is not nil
func evalDisks(disks []StorageAPI, errs []error) []StorageAPI {
	if len(errs) != len(disks) {
		errorIf(errors.New("unexpected disks/errors slice length"), "unable to evaluate internal disks")
		return nil
	}
	newDisks := make([]StorageAPI, len(disks))
	for index := range errs {
		if errs[index] == nil {
			newDisks[index] = disks[index]
		} else {
			newDisks[index] = nil
		}
	}
	return newDisks
}

// Errors specifically generated by getPartSizeFromIdx function.
var (
	errPartSizeZero  = errors.New("Part size cannot be zero")
	errPartSizeIndex = errors.New("Part index cannot be smaller than 1")
)

// getPartSizeFromIdx predicts the part size according to its index. It also
// returns -1 when totalSize is -1.
func getPartSizeFromIdx(totalSize int64, partSize int64, partIndex int) (int64, error) {
	if partSize == 0 {
		return 0, traceError(errPartSizeZero)
	}
	if partIndex < 1 {
		return 0, traceError(errPartSizeIndex)
	}
	switch totalSize {
	case -1, 0:
		return totalSize, nil
	}
	// Compute the total count of parts
	partsCount := totalSize/partSize + 1
	// Return the part's size
	switch {
	case int64(partIndex) < partsCount:
		return partSize, nil
	case int64(partIndex) == partsCount:
		// Size of last part
		return totalSize % partSize, nil
	default:
		return 0, nil
	}
}
