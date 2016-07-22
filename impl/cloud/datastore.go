// Copyright 2016 The LUCI Authors. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package cloud

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/luci/luci-go/common/errors"

	ds "github.com/luci/gae/service/datastore"
	infoS "github.com/luci/gae/service/info"
	"google.golang.org/cloud/datastore"

	"golang.org/x/net/context"
)

type cloudDatastore struct {
	client *datastore.Client
}

func (cds *cloudDatastore) use(c context.Context) context.Context {
	return ds.SetRawFactory(c, func(ic context.Context, wantTxn bool) ds.RawInterface {
		inf := infoS.Get(ic)
		if ns, ok := inf.GetNamespace(); ok {
			ic = datastore.WithNamespace(ic, ns)
		}

		bds := boundDatastore{
			Context:        ic,
			cloudDatastore: cds,
			appID:          inf.FullyQualifiedAppID(),
		}
		if wantTxn {
			bds.transaction = datastoreTransaction(ic)
		}
		return &bds
	})
}

// boundDatastore is a bound instance of the cloudDatastore installed in the
// Context.
type boundDatastore struct {
	// Context is the bound user Context. It includes the datastore namespace, if
	// one is set.
	context.Context
	*cloudDatastore

	appID       string
	transaction *datastore.Transaction
}

func (bds *boundDatastore) AllocateIDs(keys []*ds.Key, cb ds.NewKeyCB) error {
	nativeKeys, err := bds.client.AllocateIDs(bds, bds.gaeKeysToNative(keys...))
	if err != nil {
		return normalizeError(err)
	}

	keys = bds.nativeKeysToGAE(nativeKeys...)
	for _, key := range keys {
		cb(key, nil)
	}
	return nil
}

func (bds *boundDatastore) RunInTransaction(fn func(context.Context) error, opts *ds.TransactionOptions) error {
	if bds.transaction != nil {
		return errors.New("nested transactions are not supported")
	}

	// The cloud datastore SDK does not expose any transaction options.
	if opts != nil {
		switch {
		case opts.XG:
			return errors.New("cross-group transactions are not supported")
		}
	}

	attempts := 3
	if opts != nil && opts.Attempts > 0 {
		attempts = opts.Attempts
	}
	for i := 0; i < attempts; i++ {
		_, err := bds.client.RunInTransaction(bds, func(tx *datastore.Transaction) error {
			return fn(withDatastoreTransaction(bds, tx))
		})
		if err = normalizeError(err); err != ds.ErrConcurrentTransaction {
			return err
		}
	}
	return ds.ErrConcurrentTransaction
}

func (bds *boundDatastore) DecodeCursor(s string) (ds.Cursor, error) {
	cursor, err := datastore.DecodeCursor(s)
	return cursor, normalizeError(err)
}

func (bds *boundDatastore) Run(q *ds.FinalizedQuery, cb ds.RawRunCB) error {
	it := bds.client.Run(bds, bds.prepareNativeQuery(q))
	cursorFn := func() (ds.Cursor, error) {
		return it.Cursor()
	}

	for {
		var npls *nativePropertyLoadSaver
		if !q.KeysOnly() {
			npls = bds.mkNPLS(nil)
		}
		nativeKey, err := it.Next(npls)
		if err != nil {
			if err == datastore.Done {
				return nil
			}
			return normalizeError(err)
		}

		if err := cb(bds.nativeKeysToGAE(nativeKey)[0], npls.pmap, cursorFn); err != nil {
			if err == ds.Stop {
				return nil
			}
			return normalizeError(err)
		}
	}
}

func (bds *boundDatastore) Count(q *ds.FinalizedQuery) (int64, error) {
	v, err := bds.client.Count(bds, bds.prepareNativeQuery(q))
	if err != nil {
		return -1, normalizeError(err)
	}
	return int64(v), nil
}

func idxCallbacker(err error, amt int, cb func(idx int, err error) error) error {
	if err == nil {
		for i := 0; i < amt; i++ {
			if err := cb(i, nil); err != nil {
				return err
			}
		}
		return nil
	}

	err = errors.Fix(err)
	if me, ok := err.(errors.MultiError); ok {
		for i, err := range me {
			if err := cb(i, normalizeError(err)); err != nil {
				return err
			}
		}
		return nil
	}
	return normalizeError(err)
}

func (bds *boundDatastore) GetMulti(keys []*ds.Key, _meta ds.MultiMetaGetter, cb ds.GetMultiCB) error {
	nativeKeys := bds.gaeKeysToNative(keys...)
	nativePLS := make([]*nativePropertyLoadSaver, len(nativeKeys))
	for i := range nativePLS {
		nativePLS[i] = bds.mkNPLS(nil)
	}

	var err error
	if tx := bds.transaction; tx != nil {
		// Transactional GetMulti.
		err = tx.GetMulti(nativeKeys, nativePLS)
	} else {
		// Non-transactional GetMulti.
		err = bds.client.GetMulti(bds, nativeKeys, nativePLS)
	}

	return idxCallbacker(err, len(nativePLS), func(idx int, err error) error {
		return cb(nativePLS[idx].pmap, err)
	})
}

func (bds *boundDatastore) PutMulti(keys []*ds.Key, vals []ds.PropertyMap, cb ds.NewKeyCB) error {
	nativeKeys := bds.gaeKeysToNative(keys...)
	nativePLS := make([]*nativePropertyLoadSaver, len(vals))
	for i := range nativePLS {
		nativePLS[i] = bds.mkNPLS(vals[i])
	}

	var err error
	if tx := bds.transaction; tx != nil {
		// Transactional PutMulti.
		//
		// In order to simulate the presence of mid-transaction key allocation, we
		// will identify any incomplete keys and allocate IDs for them. This is
		// potentially wasteful in the event of failed or retried transactions, but
		// it is required to maintain API compatibility with the datastore
		// interface.
		var incompleteKeys []*datastore.Key
		var incompleteKeyMap map[int]int
		for i, k := range nativeKeys {
			if k.Incomplete() {
				if incompleteKeyMap == nil {
					// Optimization: if there are any incomplete keys, allocate room for
					// the full range.
					incompleteKeyMap = make(map[int]int, len(nativeKeys)-i)
					incompleteKeys = make([]*datastore.Key, 0, len(nativeKeys)-i)
				}
				incompleteKeyMap[len(incompleteKeys)] = i
				incompleteKeys = append(incompleteKeys, k)
			}
		}
		if len(incompleteKeys) > 0 {
			idKeys, err := bds.client.AllocateIDs(bds, incompleteKeys)
			if err != nil {
				return err
			}
			for i, idKey := range idKeys {
				nativeKeys[incompleteKeyMap[i]] = idKey
			}
		}

		_, err = tx.PutMulti(nativeKeys, nativePLS)
	} else {
		// Non-transactional PutMulti.
		nativeKeys, err = bds.client.PutMulti(bds, nativeKeys, nativePLS)
	}

	return idxCallbacker(err, len(nativeKeys), func(idx int, err error) error {
		if err == nil {
			return cb(bds.nativeKeysToGAE(nativeKeys[idx])[0], nil)
		}
		return cb(nil, err)
	})
}

func (bds *boundDatastore) DeleteMulti(keys []*ds.Key, cb ds.DeleteMultiCB) error {
	nativeKeys := bds.gaeKeysToNative(keys...)

	var err error
	if tx := bds.transaction; tx != nil {
		// Transactional DeleteMulti.
		err = tx.DeleteMulti(nativeKeys)
	} else {
		// Non-transactional DeleteMulti.
		err = bds.client.DeleteMulti(bds, nativeKeys)
	}

	return idxCallbacker(err, len(nativeKeys), func(_ int, err error) error {
		return cb(err)
	})
}

func (bds *boundDatastore) Testable() ds.Testable {
	return nil
}

func (bds *boundDatastore) prepareNativeQuery(fq *ds.FinalizedQuery) *datastore.Query {
	nq := datastore.NewQuery(fq.Kind())
	if bds.transaction != nil {
		nq = nq.Transaction(bds.transaction)
	}

	// nativeFilter translates a filter field. If the translation fails, we'll
	// pass the result through to the underlying datastore and allow it to
	// reject it.
	nativeFilter := func(prop ds.Property) interface{} {
		if np, err := bds.gaePropertyToNative("", []ds.Property{prop}); err == nil {
			return np.Value
		}
		return prop.Value()
	}

	// Equality filters.
	for field, props := range fq.EqFilters() {
		for _, prop := range props {
			nq = nq.Filter(fmt.Sprintf("%s =", field), nativeFilter(prop))
		}
	}

	// Inequality filters.
	if ineq := fq.IneqFilterProp(); ineq != "" {
		if field, op, prop := fq.IneqFilterLow(); field != "" {
			nq = nq.Filter(fmt.Sprintf("%s %s", field, op), nativeFilter(prop))
		}

		if field, op, prop := fq.IneqFilterHigh(); field != "" {
			nq = nq.Filter(fmt.Sprintf("%s %s", field, op), nativeFilter(prop))
		}
	}

	start, end := fq.Bounds()
	if start != nil {
		nq = nq.Start(start.(datastore.Cursor))
	}
	if end != nil {
		nq = nq.End(end.(datastore.Cursor))
	}

	if fq.Distinct() {
		nq = nq.Distinct()
	}
	if fq.KeysOnly() {
		nq = nq.KeysOnly()
	}
	if limit, ok := fq.Limit(); ok {
		nq = nq.Limit(int(limit))
	}
	if offset, ok := fq.Offset(); ok {
		nq = nq.Offset(int(offset))
	}
	if proj := fq.Project(); proj != nil {
		nq = nq.Project(proj...)
	}
	if ancestor := fq.Ancestor(); ancestor != nil {
		nq = nq.Ancestor(bds.gaeKeysToNative(ancestor)[0])
	}
	if fq.EventuallyConsistent() {
		nq = nq.EventualConsistency()
	}

	for _, ic := range fq.Orders() {
		prop := ic.Property
		if ic.Descending {
			prop = "-" + prop
		}
		nq = nq.Order(prop)
	}

	return nq
}

func (bds *boundDatastore) mkNPLS(base ds.PropertyMap) *nativePropertyLoadSaver {
	return &nativePropertyLoadSaver{bds: bds, pmap: clonePropertyMap(base)}
}

func (bds *boundDatastore) gaePropertyToNative(name string, props []ds.Property) (nativeProp datastore.Property, err error) {
	nativeProp.Name = name

	nativeValues := make([]interface{}, len(props))
	for i, prop := range props {
		switch pt := prop.Type(); pt {
		case ds.PTNull, ds.PTInt, ds.PTTime, ds.PTBool, ds.PTBytes, ds.PTString, ds.PTFloat:
			nativeValues[i] = prop.Value()
			break

		case ds.PTKey:
			nativeValues[i] = bds.gaeKeysToNative(prop.Value().(*ds.Key))[0]

		default:
			err = fmt.Errorf("unsupported property type at %d: %v", i, pt)
			return
		}
	}

	if len(nativeValues) == 1 {
		nativeProp.Value = nativeValues[0]
		nativeProp.NoIndex = (props[0].IndexSetting() != ds.ShouldIndex)
	} else {
		// We must always index list values.
		nativeProp.Value = nativeValues
	}
	return
}

func (bds *boundDatastore) nativePropertyToGAE(nativeProp datastore.Property) (name string, props []ds.Property, err error) {
	name = nativeProp.Name

	var nativeValues []interface{}
	// Slice of supported native type. Break this into a slice of datastore
	// properties.
	//
	// It must be an []interface{}.
	if rv := reflect.ValueOf(nativeProp.Value); rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Interface {
		nativeValues = rv.Interface().([]interface{})
	} else {
		nativeValues = []interface{}{nativeProp.Value}
	}

	if len(nativeValues) == 0 {
		return
	}

	props = make([]ds.Property, len(nativeValues))
	for i, nv := range nativeValues {
		switch nvt := nv.(type) {
		case int64, bool, string, float64, []byte:
			break

		case time.Time:
			// Cloud datastore library returns local time.
			nv = nvt.UTC()

		case *datastore.Key:
			nv = bds.nativeKeysToGAE(nvt)[0]

		default:
			err = fmt.Errorf("element %d has unsupported datastore.Value type %T", i, nv)
			return
		}

		indexSetting := ds.ShouldIndex
		if nativeProp.NoIndex {
			indexSetting = ds.NoIndex
		}
		props[i].SetValue(nv, indexSetting)
	}
	return
}

func (bds *boundDatastore) gaeKeysToNative(keys ...*ds.Key) []*datastore.Key {
	nativeKeys := make([]*datastore.Key, len(keys))
	for i, key := range keys {
		_, _, toks := key.Split()

		var nativeKey *datastore.Key
		for _, tok := range toks {
			nativeKey = datastore.NewKey(bds, tok.Kind, tok.StringID, tok.IntID, nativeKey)
		}
		nativeKeys[i] = nativeKey
	}
	return nativeKeys
}

func (bds *boundDatastore) nativeKeysToGAE(nativeKeys ...*datastore.Key) []*ds.Key {
	keys := make([]*ds.Key, len(nativeKeys))
	toks := make([]ds.KeyTok, 1)
	for i, nativeKey := range nativeKeys {
		toks = toks[:0]
		cur := nativeKey
		for {
			toks = append(toks, ds.KeyTok{Kind: cur.Kind(), IntID: cur.ID(), StringID: cur.Name()})
			cur = cur.Parent()
			if cur == nil {
				break
			}
		}

		// Reverse "toks" so we have ancestor-to-child lineage.
		for i := 0; i < len(toks)/2; i++ {
			ri := len(toks) - i - 1
			toks[i], toks[ri] = toks[ri], toks[i]
		}
		keys[i] = ds.NewKeyToks(bds.appID, nativeKey.Namespace(), toks)
	}
	return keys
}

// nativePropertyLoadSaver is a ds.PropertyMap which implements
// datastore.PropertyLoadSaver.
//
// It naturally converts between native and GAE properties and values.
type nativePropertyLoadSaver struct {
	bds  *boundDatastore
	pmap ds.PropertyMap
}

var _ datastore.PropertyLoadSaver = (*nativePropertyLoadSaver)(nil)

func (npls *nativePropertyLoadSaver) Load(props []datastore.Property) error {
	if npls.pmap == nil {
		// Allocate for common case: one property per property name.
		npls.pmap = make(ds.PropertyMap, len(props))
	}

	for _, nativeProp := range props {
		name, props, err := npls.bds.nativePropertyToGAE(nativeProp)
		if err != nil {
			return err
		}
		npls.pmap[name] = append(npls.pmap[name], props...)
	}
	return nil
}

func (npls *nativePropertyLoadSaver) Save() ([]datastore.Property, error) {
	if len(npls.pmap) == 0 {
		return nil, nil
	}

	props := make([]datastore.Property, 0, len(npls.pmap))
	for name, plist := range npls.pmap {
		// Strip meta.
		if strings.HasPrefix(name, "$") {
			continue
		}

		nativeProp, err := npls.bds.gaePropertyToNative(name, plist)
		if err != nil {
			return nil, err
		}
		props = append(props, nativeProp)
	}
	return props, nil
}

var datastoreTransactionKey = "*datastore.Transaction"

func withDatastoreTransaction(c context.Context, tx *datastore.Transaction) context.Context {
	return context.WithValue(c, &datastoreTransactionKey, tx)
}

func datastoreTransaction(c context.Context) *datastore.Transaction {
	if tx, ok := c.Value(&datastoreTransactionKey).(*datastore.Transaction); ok {
		return tx
	}
	return nil
}

func clonePropertyMap(pmap ds.PropertyMap) ds.PropertyMap {
	if pmap == nil {
		return nil
	}

	clone := make(ds.PropertyMap, len(pmap))
	for k, props := range pmap {
		clone[k] = append([]ds.Property(nil), props...)
	}
	return clone
}

func normalizeError(err error) error {
	switch err {
	case datastore.ErrNoSuchEntity:
		return ds.ErrNoSuchEntity
	case datastore.ErrConcurrentTransaction:
		return ds.ErrConcurrentTransaction
	case datastore.ErrInvalidKey:
		return ds.ErrInvalidKey
	default:
		return err
	}
}