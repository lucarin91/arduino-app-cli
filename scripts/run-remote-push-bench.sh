#!/bin/sh
# This file is part of arduino-app-cli.
#
# SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
# SPDX-License-Identifier: GPL-3.0-or-later

go test -bench='BenchmarkRemotePush/Native' -run='^$' -count=10 -benchmem ./pkg/board/remote/... | tee native.bench
go test -bench='BenchmarkRemotePush/FSWalk' -run='^$' -count=10 -benchmem ./pkg/board/remote/... | tee fswalk.bench

gsed -i 's/\/Native//g' native.bench
gsed -i 's/\/FSWalk//g' fswalk.bench

benchstat fswalk.bench  native.bench
