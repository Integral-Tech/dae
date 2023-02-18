/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, v2rayA Organization <team@v2raya.org>
 */

package domain_matcher

import (
	"fmt"
	"github.com/openacid/slim/encode"
	"github.com/openacid/slim/trie"
	"github.com/v2rayA/ahocorasick-domain"
	"github.com/v2rayA/dae/common/consts"
	"regexp"
	"sort"
	"strings"
)

type AhocorasickSuccinctset struct {
	validAcIndexes     []int
	validTrieIndexes   []int
	validRegexpIndexes []int
	ac                 []*ahocorasick.Matcher
	trie               []*trie.SlimTrie
	regexp             [][]*regexp.Regexp

	toBuildAc   [][][]byte
	toBuildTrie [][]string
	err         error
}

func NewAhocorasickSuccinctset(bitLength int) *AhocorasickSuccinctset {
	return &AhocorasickSuccinctset{
		ac:          make([]*ahocorasick.Matcher, bitLength),
		trie:        make([]*trie.SlimTrie, bitLength),
		regexp:      make([][]*regexp.Regexp, bitLength),
		toBuildAc:   make([][][]byte, bitLength),
		toBuildTrie: make([][]string, bitLength),
	}
}
func (n *AhocorasickSuccinctset) AddSet(bitIndex int, patterns []string, typ consts.RoutingDomainKey) {
	if n.err != nil {
		return
	}
	switch typ {
	case consts.RoutingDomainKey_Full:
		for _, d := range patterns {
			n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], "^"+d+"$")
		}
	case consts.RoutingDomainKey_Suffix:
		for _, d := range patterns {
			if strings.HasPrefix(d, ".") {
				// abc.example.com
				n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], d+"$")
				// cannot match example.com
			} else {
				// xxx.example.com
				n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], "."+d+"$")
				// example.com
				n.toBuildTrie[bitIndex] = append(n.toBuildTrie[bitIndex], "^"+d+"$")
				// cannot match abcexample.com
			}
		}
	case consts.RoutingDomainKey_Keyword:
		// Only use ac automaton for "keyword" matching to save memory.
		for _, d := range patterns {
			n.toBuildAc[bitIndex] = append(n.toBuildAc[bitIndex], []byte(d))
		}
	case consts.RoutingDomainKey_Regex:
		for _, d := range patterns {
			r, err := regexp.Compile(d)
			if err != nil {
				n.err = fmt.Errorf("failed to compile regex: %v", d)
				return
			}
			n.regexp[bitIndex] = append(n.regexp[bitIndex], r)
		}
	default:
		n.err = fmt.Errorf("unknown RoutingDomainKey: %v", typ)
		return
	}
}
func (n *AhocorasickSuccinctset) MatchDomainBitmap(domain string) (bitmap []uint32) {
	N := len(n.ac) / 32
	if len(n.ac)%32 != 0 {
		N++
	}
	bitmap = make([]uint32, N)
	// Add magic chars as head and tail.
	domain = "^" + strings.ToLower(strings.TrimSuffix(domain, ".")) + "$"
	// Domain should consist of 'a'-'z' and '.' and '-'
	for _, b := range []byte(domain) {
		if !ahocorasick.IsValidChar(b) {
			return bitmap
		}
	}
	// Suffix matching.
	suffixTrieDomain := ToSuffixTrieString(domain)
	for _, i := range n.validTrieIndexes {
		if bitmap[i/32]&(1<<(i%32)) > 0 {
			// Already matched.
			continue
		}
		if _, ok := n.trie[i].Get(suffixTrieDomain); ok {
			bitmap[i/32] |= 1 << (i % 32)
		}
	}
	// Keyword matching.
	for _, i := range n.validAcIndexes {
		if bitmap[i/32]&(1<<(i%32)) > 0 {
			// Already matched.
			continue
		}
		if hits := n.ac[i].MatchThreadSafe([]byte(domain)); len(hits) > 0 {
			bitmap[i/32] |= 1 << (i % 32)
		}
	}
	// Regex matching.
	for _, i := range n.validRegexpIndexes {
		if bitmap[i/32]&(1<<(i%32)) > 0 {
			// Already matched.
			continue
		}
		for _, r := range n.regexp[i] {
			if r.MatchString(domain) {
				bitmap[i/32] |= 1 << (i % 32)
				break
			}
		}
	}
	return bitmap
}
func ToSuffixTrieString(s string) string {
	// No need for end char "$".
	b := []byte(strings.TrimSuffix(s, "$"))
	// Reverse.
	half := len(b) / 2
	for i := 0; i < half; i++ {
		b[i], b[len(b)-i-1] = b[len(b)-i-1], b[i]
	}
	return string(b)
}
func ToSuffixTrieStrings(s []string) []string {
	to := make([]string, len(s))
	for i := range s {
		to[i] = ToSuffixTrieString(s[i])
	}
	return to
}
func (n *AhocorasickSuccinctset) Build() (err error) {
	if n.err != nil {
		return n.err
	}
	n.validAcIndexes = make([]int, 0, len(n.toBuildAc)/8)
	n.validTrieIndexes = make([]int, 0, len(n.toBuildAc)/8)
	n.validRegexpIndexes = make([]int, 0, len(n.toBuildAc)/8)
	// Build AC automaton.
	for i, toBuild := range n.toBuildAc {
		if len(toBuild) == 0 {
			continue
		}
		n.ac[i], err = ahocorasick.NewMatcher(toBuild)
		if err != nil {
			return err
		}
		n.validAcIndexes = append(n.validAcIndexes, i)
	}

	// Build succinct trie.
	for i, toBuild := range n.toBuildTrie {
		if len(toBuild) == 0 {
			continue
		}
		toBuild = ToSuffixTrieStrings(toBuild)
		sort.Strings(toBuild)
		n.trie[i], err = trie.NewSlimTrie(encode.I8{}, toBuild, nil)
		if err != nil {
			return err
		}
		n.validTrieIndexes = append(n.validTrieIndexes, i)
	}

	// Regexp.
	for i := range n.regexp {
		if len(n.regexp[i]) == 0 {
			continue
		}
		n.validRegexpIndexes = append(n.validRegexpIndexes, i)
	}

	// Release unused data.
	n.toBuildAc = nil
	n.toBuildTrie = nil
	return nil
}