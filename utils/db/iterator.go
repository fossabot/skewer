package db

import (
	"github.com/awnumar/memguard"
	"github.com/dgraph-io/badger"
	"github.com/stephane-martin/skewer/utils"
	"github.com/stephane-martin/skewer/utils/sbox"
)

type ULIDIterator struct {
	iter   *badger.Iterator
	prefix []byte
	secret *memguard.LockedBuffer
}

func (i *ULIDIterator) Close() {
	i.iter.Close()
}

func (i *ULIDIterator) Next() {
	i.iter.Next()
}

func (i *ULIDIterator) Rewind() {
	if i.prefix != nil {
		i.iter.Seek(i.prefix)
	} else {
		i.iter.Rewind()
	}
}

func (i *ULIDIterator) Valid() bool {
	if i.prefix != nil {
		return i.iter.ValidForPrefix(i.prefix)
	} else {
		return i.iter.Valid()
	}
}

func (i *ULIDIterator) Key() (uid utils.MyULID) {
	if i.prefix != nil {
		copy(uid[:], i.iter.Item().Key()[len(i.prefix):])
	} else {
		copy(uid[:], i.iter.Item().Key())
	}
	return uid
}

func (i *ULIDIterator) KeyInto(uid *utils.MyULID) bool {
	if i == nil || uid == nil {
		return false
	}
	if i.prefix != nil {
		copy((*uid)[:], i.iter.Item().Key()[len(i.prefix):])
	} else {
		copy((*uid)[:], i.iter.Item().Key())
	}
	return true
}

func (i *ULIDIterator) Value() ([]byte, error) {
	if i.secret == nil {
		return i.iter.Item().Value()
	}
	var err error
	var encVal []byte
	var decVal []byte
	encVal, err = i.iter.Item().Value()
	if err != nil {
		return nil, err
	}
	if encVal == nil {
		return nil, nil
	}
	decVal, err = sbox.Decrypt(encVal, i.secret)
	if err != nil {
		return nil, err
	}
	return decVal, nil
}

func (i *ULIDIterator) ValueCopy(dst []byte) ([]byte, error) {
	if i.secret == nil {
		return i.iter.Item().ValueCopy(dst)
	}
	var err error
	var encVal []byte
	var decVal []byte
	encVal, err = i.iter.Item().Value()
	if err != nil {
		return nil, err
	}
	if encVal == nil {
		return nil, nil
	}
	decVal, err = sbox.Decrypt(encVal, i.secret)
	if err != nil {
		return nil, err
	}
	return append(dst[:0], decVal...), nil

}
