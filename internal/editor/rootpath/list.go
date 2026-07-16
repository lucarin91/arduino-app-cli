// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package rootpath

import "sort"

// PathList is a slice of *Path with helpers mirroring paths.PathList.
type PathList []*Path

func (l *PathList) Add(p *Path) { *l = append(*l, p) }

func (l PathList) Sort() {
	sort.Slice(l, func(i, j int) bool { return l[i].rel < l[j].rel })
}

func (l PathList) ContainsEquivalentTo(p *Path) bool {
	for _, x := range l {
		if x.EquivalentTo(p) {
			return true
		}
	}
	return false
}
