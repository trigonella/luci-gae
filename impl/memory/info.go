// Copyright 2015 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package memory

import (
	"fmt"
	"regexp"

	"github.com/luci/gae/impl/dummy"
	"github.com/luci/gae/service/info"
	"golang.org/x/net/context"
)

type giContextKeyType int

var giContextKey giContextKeyType

// validNamespace matches valid namespace names.
var validNamespace = regexp.MustCompile(`^[0-9A-Za-z._-]{0,100}$`)

var defaultGlobalInfoData = globalInfoData{
	// versionID returns X.Y where Y is autogenerated by appengine, and X is
	// whatever's in app.yaml.
	versionID: "testVersionID.1",
	requestID: "test-request-id",
}

type globalInfoData struct {
	appID     string
	fqAppID   string
	namespace *string
	versionID string
	requestID string
}

func (gid *globalInfoData) getNamespace() (string, bool) {
	if ns := gid.namespace; ns != nil {
		return *ns, true
	}
	return "", false
}

func curGID(c context.Context) *globalInfoData {
	if gid, ok := c.Value(giContextKey).(*globalInfoData); ok {
		return gid
	}
	return &defaultGlobalInfoData
}

func useGID(c context.Context, f func(mod *globalInfoData)) context.Context {
	cur := curGID(c)
	if cur == nil {
		cur = &defaultGlobalInfoData
	}

	clone := *cur
	f(&clone)
	return context.WithValue(c, giContextKey, &clone)
}

// useGI adds a gae.GlobalInfo context, accessible
// by gae.GetGI(c)
func useGI(c context.Context) context.Context {
	return info.SetFactory(c, func(ic context.Context) info.Interface {
		return &giImpl{dummy.Info(), curGID(ic), ic}
	})
}

type giImpl struct {
	info.Interface
	*globalInfoData
	c context.Context
}

var _ = info.Interface((*giImpl)(nil))

func (gi *giImpl) GetNamespace() (string, bool) {
	return gi.getNamespace()
}

func (gi *giImpl) Namespace(ns string) (ret context.Context, err error) {
	if !validNamespace.MatchString(ns) {
		return nil, fmt.Errorf("appengine: namespace %q does not match /%s/", ns, validNamespace)
	}

	return useGID(gi.c, func(mod *globalInfoData) {
		mod.namespace = &ns
	}), nil
}

func (gi *giImpl) MustNamespace(ns string) context.Context {
	ret, err := gi.Namespace(ns)
	if err != nil {
		panic(err)
	}
	return ret
}

func (gi *giImpl) AppID() string {
	return gi.appID
}

func (gi *giImpl) FullyQualifiedAppID() string {
	return gi.fqAppID
}

func (gi *giImpl) IsDevAppServer() bool {
	return true
}

func (gi *giImpl) VersionID() string {
	return curGID(gi.c).versionID
}

func (gi *giImpl) RequestID() string {
	return curGID(gi.c).requestID
}

func (gi *giImpl) Testable() info.Testable {
	return gi
}

func (gi *giImpl) SetVersionID(v string) context.Context {
	return useGID(gi.c, func(mod *globalInfoData) {
		mod.versionID = v
	})
}

func (gi *giImpl) SetRequestID(v string) context.Context {
	return useGID(gi.c, func(mod *globalInfoData) {
		mod.requestID = v
	})
}
