/*
Package pmap provides persistent dictionaries with high-performance access and specific timestamp semantics.
*/
package pmap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"time"
	"github.com/dv343/treeless/hashing"
)

//FilePerms i
const FilePerms = 0700

const defaultCheckSumInterval = time.Second

/*
A PMap is a persistent dictionary with high-performance access and specific timestamp semantics.

A PMap has some utility functions and 4 primitives: Get, Set, Del and CAS.

Set, del and cas value parameters should contain an 8 byte long header with a timestamp.
Get returns a similar value (header + real value).
These timestamps have specific semantics described in each operation.

They are composed by a hashmap and a list:
-The hashmap is stored in memory (RAM-only). It is used to index key-value pairs.
It uses 8 bytes per bucket and it is expanded at twice its size each time a load factor is reached.
-The list is stored in a memory-mapped file, RAM vs disk usage is controlled by
kernel. It uses an 8 byte long header.

Note: this module is *not* thread-safe.
*/
type PMap struct {
	hm       *hashmap
	st       *store
	checksum syncChecksum
	path     string
}

//New returns an initialized PMap stored in path with a maximum store size.
//Set path to "" to make the PMap anonymous, it will use RAM for everything and it won't use the file system.
func New(path string, size uint64) *PMap {
	c := new(PMap)
	c.path = path
	c.hm = newHashMap(defaultHashMapInitialLog2Size, defaultHashMapSizeLimit)
	c.st = newStore(c.path, size)
	//c.checksum.SetInterval(defaultCheckSumInterval)
	return c
}

//Open opens a previous closed pmap returning a new pmap
func Open(path string) *PMap {
	c := new(PMap)
	c.path = path
	c.hm = newHashMap(defaultHashMapInitialLog2Size, defaultHashMapSizeLimit)
	c.st = openStore(c.path)
	//Restore every pair, introduce all pairs into the hashmap and calculate deleted bytes and length of the opened store
	for index := uint64(0); ; {
		if c.st.keyLen(index) <= 0 {
			break
		}
		//if not 2 totallen => corrupt=> break
		key := c.st.key(index)
		val := c.st.val(index)
		c.restorePair(key, val, uint32(index))

		if len(val) > 0 {
		} else {
			c.st.deleted += uint64(12 + len(key))
		}

		index += 12 + uint64(c.st.totalLen(index))
		c.st.length = index
	}
	//c.checksum.SetInterval(defaultCheckSumInterval)
	return c
}

//This function is only used to restore the PMap after a DB close
func (c *PMap) restorePair(key, value []byte, storeIndex uint32) error {
	//Check for available space
	if c.hm.numStoredKeys >= c.hm.numKeysToExpand {
		err := c.hm.expand()
		if err != nil {
			return err
		}
	}
	h64 := hashing.FNV1a64(key)
	h := hashReMap(uint32(h64))
	index := h & c.hm.sizeMask
	col := 0
	for {
		storedHash := c.hm.getHash(index)
		if storedHash == emptyBucket {
			//Empty bucket: put the pair
			c.hm.setHash(index, h)
			c.hm.setStoreIndex(index, storeIndex)
			c.hm.numStoredKeys++
			t := time.Unix(0, int64(binary.LittleEndian.Uint64(value[:8])))
			//fmt.Println("Sum", value)
			c.checksum.sum(h64^binary.LittleEndian.Uint64(value[:8]), t)
			return nil
		}
		if h == storedHash {
			col++
			if col > 2 {
				fmt.Println("COL", col)
			}
			//Same hash: perform full key comparison
			stIndex := c.hm.getStoreIndex(index)
			storedKey := c.st.key(uint64(stIndex))
			if bytes.Equal(storedKey, key) {
				//Full match, the key was in the map
				//Last write wins
				v := c.st.val(uint64(stIndex))
				t := time.Unix(0, int64(binary.LittleEndian.Uint64(value[:8])))
				c.checksum.sub(h64^binary.LittleEndian.Uint64(v[:8]), t)
				//fmt.Println("Sub", v)
				c.st.deleted += uint64(12 + len(key) + len(v))
				if len(value) > 0 {
					c.hm.setHash(index, h)
					c.hm.setStoreIndex(index, storeIndex)
					c.checksum.sum(h64^binary.LittleEndian.Uint64(value[:8]), t)
					//fmt.Println("Sum2", value)
				} else {
					c.hm.setHash(index, deletedBucket)
				}
				return nil
			}
		}
		index = (index + 1) & c.hm.sizeMask
	}
}

//Checksum returns a time-stable checksum
func (c *PMap) Checksum() uint64 {
	return c.checksum.checksum()
}

//Close closes a PMap. The hashmap is destroyed and the store is disk synced.
//Close will panic if it is called more than one time.
func (c *PMap) Close() {
	c.st.close()
}

//CloseAndDelete closes the PMap and removes the associated file freeing disk space.
func (c *PMap) CloseAndDelete() {
	c.st.close()
	c.st.deleteStore()
}

//Deleted returns the number of bytes deleted
func (c *PMap) Deleted() int {
	return int(c.st.deleted)
}

//Used returns the number of bytes used
func (c *PMap) Used() int {
	return int(c.st.length)
}

//Size returns the size of the pmap
func (c *PMap) Size() int {
	return int(c.st.size)
}

/*
	Primitives
*/

//Get returns the key's associated value or nil if it doesn't exists (or was deleted)
//If the pair doesn't exist it will return (nil, nil), non-existance is not considered an error
//The first 8 bytes contain the timestamp of the pair (nanoseconds elapsed since Unix time).
//Returned value is a copy of the stored one
func (c *PMap) Get(h32 uint32, key []byte) ([]byte, error) {
	h := uint32(h32)
	//Search for the key by using open adressing with linear probing
	index := h & c.hm.sizeMask
	for {
		storedHash := c.hm.getHash(index)
		if storedHash == emptyBucket {
			return nil, nil
		} else if h == storedHash {
			//Same hash: perform full key comparison
			stIndex := c.hm.getStoreIndex(index)
			storedKey := c.st.key(uint64(stIndex))
			if bytes.Equal(storedKey, key) {
				//Full match, the key was in the map
				v := c.st.val(uint64(stIndex))
				//We need to copy the value, returning a memory mapped file slice is dangerous,
				//the mutex wont be hold after this function returns
				vc := make([]byte, len(v))
				copy(vc, v)
				return vc, nil
			}
		}
		index = (index + 1) & c.hm.sizeMask
	}
}

//Set sets the value of a pair if the pair doesn't exists or if
//the already stored pair timestamp is before the provided timestamp.
//The first 8 bytes of value should contain the timestamp of the pair (nanoseconds elapsed since Unix time).
func (c *PMap) Set(h64 uint64, key, value []byte) error {
	if len(value) < 8 {
		return errors.New(("Error: message value len < 8"))
	}
	//Check for available space
	if c.hm.numStoredKeys >= c.hm.numKeysToExpand {
		err := c.hm.expand()
		if err != nil {
			return err
		}
	}

	h := hashReMap(uint32(h64))
	index := h & c.hm.sizeMask
	col := 0
	for {
		storedHash := c.hm.getHash(index)
		if storedHash == emptyBucket {
			//Empty bucket: put the pair
			storeIndex, err := c.st.put(key, value)
			if err != nil {
				return err
			}
			c.hm.setHash(index, h)
			c.hm.setStoreIndex(index, storeIndex)
			c.hm.numStoredKeys++
			t := time.Unix(0, int64(binary.LittleEndian.Uint64(value[:8])))
			c.checksum.sum(h64^binary.LittleEndian.Uint64(value[:8]), t)
			return nil
		}

		if h == storedHash {
			col++
			if col > 2 {
				fmt.Println("COL", col)
			}
			//Same hash: perform full key comparison
			stIndex := c.hm.getStoreIndex(index)
			storedKey := c.st.key(uint64(stIndex))
			if bytes.Equal(storedKey, key) {
				//Full match, the key was in the map
				//Last write wins
				v := c.st.val(uint64(stIndex))
				oldT := time.Unix(0, int64(binary.LittleEndian.Uint64(v[:8])))
				t := time.Unix(0, int64(binary.LittleEndian.Uint64(value[:8])))
				if oldT.After(t) || oldT.Equal(t) {
					//Stored pair is newer than the provided pair
					//fmt.Println("Discarded", key, value, t)
					return nil
				}
				storeIndex, err := c.st.put(key, value)
				if err != nil {
					return err
				}
				c.checksum.sub(h64^binary.LittleEndian.Uint64(v[:8]), t)
				c.hm.setHash(index, h)
				c.hm.setStoreIndex(index, storeIndex)
				c.checksum.sum(h64^binary.LittleEndian.Uint64(value[:8]), t)
				return nil
			}
		}
		index = (index + 1) & c.hm.sizeMask
	}
}

//CAS (compare and swap) sets a pair value if 2 tests are passed.
//The value should be in this format:
//[0:8]   => CAS timestamp
//[8:16]  => old value FNV1a64 hash
//[16:24] => new timestamp
//[24:]   => new value
//Tests:
//1. Stored value timestamp match the CAS timestamp, if the pair doesn't exists the CAS timestamp should be 0
//2. Stored value hash matches the provided hash
//It returns nil if the new value was written
func (c *PMap) CAS(h64 uint64, key, value []byte) error {
	if len(value) < 24 {
		return errors.New("Error: CAS value len < 16")
	}
	//Check for available space
	if c.hm.numStoredKeys >= c.hm.numKeysToExpand {
		err := c.hm.expand()
		if err != nil {
			return err
		}
	}

	providedTime := time.Unix(0, int64(binary.LittleEndian.Uint64(value[:8])))
	hv := binary.LittleEndian.Uint64(value[8:16])
	t := time.Unix(0, int64(binary.LittleEndian.Uint64(value[16:24])))
	//fmt.Println(t.UnixNano())
	h := hashReMap(uint32(h64))
	index := h & c.hm.sizeMask
	for {
		storedHash := c.hm.getHash(index)
		if storedHash == emptyBucket {
			//Empty bucket: put the pair
			if !providedTime.Equal(time.Unix(0, 0)) && hv != hashing.FNV1a64(nil) {
				return errors.New("CAS failed: empty pair: non-zero timestamp")
			}
			storeIndex, err := c.st.put(key, value[16:])
			if err != nil {
				return err
			}
			c.hm.setHash(index, h)
			c.hm.setStoreIndex(index, storeIndex)
			c.hm.numStoredKeys++
			c.checksum.sum(h64^binary.LittleEndian.Uint64(value[16:24]), t)
			return nil
		}
		if h == storedHash {
			//Same hash: perform full key comparison
			stIndex := c.hm.getStoreIndex(index)
			storedKey := c.st.key(uint64(stIndex))
			if bytes.Equal(storedKey, key) {
				//Full match, the key was in the map
				v := c.st.val(uint64(stIndex))
				oldT := time.Unix(0, int64(binary.LittleEndian.Uint64(v[:8])))
				if t.Equal(oldT) {
					log.Println("Equal times!")
				}
				if oldT != providedTime {
					return errors.New("CAS failed: timestamp mismatch")
				}
				if hv != hashing.FNV1a64(v[8:]) {
					log.Println("hash mismatch!")
					return errors.New("CAS failed: hash mismatch")
				}
				c.checksum.sub(h64^binary.LittleEndian.Uint64(v[:8]), t)
				storeIndex, err := c.st.put(key, value[16:])
				if err != nil {
					return err
				}
				c.hm.setHash(index, h)
				c.hm.setStoreIndex(index, storeIndex)
				c.checksum.sum(h64^binary.LittleEndian.Uint64(value[16:24]), t)
				return nil
			}
		}
		index = (index + 1) & c.hm.sizeMask
	}
}

//Del marks as deleted a pair, future read instructions won't see the old value.
//However, it never frees the memory-mapped region associated with the deleted pair.
//It "leaks". The only way to free those regions is to delete the entire PMap.
func (c *PMap) Del(h64 uint64, key, value []byte) error {
	h := hashReMap(uint32(h64))

	//Search for the key by using open adressing with linear probing
	index := h & c.hm.sizeMask
	for {
		storedHash := c.hm.getHash(index)
		if storedHash == emptyBucket {
			return nil
		}
		if h == storedHash {
			//Same hash: perform full key comparison
			stIndex := c.hm.getStoreIndex(index)
			storedKey := c.st.key(uint64(stIndex))
			if bytes.Equal(storedKey, key) {
				//Full match, the key was in the map

				//Last write wins
				v := c.st.val(uint64(stIndex))
				oldT := time.Unix(0, int64(binary.LittleEndian.Uint64(v[:8])))
				t := time.Unix(0, int64(binary.LittleEndian.Uint64(value[:8])))
				if t.Before(oldT) {
					//Stored pair is newer than the provided pair
					return nil
				}
				c.st.deleted += uint64(12 + len(key) + len(v))
				c.checksum.sub(h64^binary.LittleEndian.Uint64(v[:8]), t)
				c.hm.setHash(index, deletedBucket)
				//Tombstone
				_, err := c.st.put(key, nil)
				return err
			}
		}
		index = (index + 1) & c.hm.sizeMask
	}
}

func (c *PMap) isPresent(index uint64) bool {
	key := c.st.key(index)
	h32 := uint32(hashing.FNV1a64(key))
	value, err := c.Get(h32, key)
	if value == nil || err != nil {
		return false
	}
	storeValue := c.st.val(index)
	return bytes.Compare(value[0:8], storeValue[0:8]) == 0
}

//BackwardsIterate calls foreach for each stored pair in backwards direction, it will stop iterating if the call returns false
//It stops early if foreach returns false
func (c *PMap) BackwardsIterate(foreach func(key, value []byte) (Continue bool)) error {
	index := c.st.length
	if index <= 0 {
		return nil
	}
	prev := uint64(c.st.prev(index))
	if prev < 0 {
		return nil
	}
	index = uint64(prev)
	for index >= 0 {
		if c.isPresent(index) {
			key := c.st.key(index)
			val := c.st.val(index)
			kc := make([]byte, len(key))
			vc := make([]byte, len(val))
			copy(kc, key)
			copy(vc, val)
			ok := foreach(kc, vc)
			if !ok {
				break
			}
		}
		prev := c.st.prev(index)
		if prev < 0 {
			break
		}
		index = uint64(prev)
	}
	return nil
}

//BackwardsIterate calls foreach for each stored pair, it will stop iterating if the call returns false
//It stops early if foreach returns false
func (c *PMap) Iterate(foreach func(key, value []byte) (Continue bool)) error {
	for index := uint64(0); index < c.st.length; {
		if c.isPresent(index) {
			key := c.st.key(index)
			val := c.st.val(index)
			kc := make([]byte, len(key))
			vc := make([]byte, len(val))
			copy(kc, key)
			copy(vc, val)
			ok := foreach(kc, vc)
			if !ok {
				break
			}
		}
		index += 12 + uint64(c.st.totalLen(index))
	}
	return nil
}
