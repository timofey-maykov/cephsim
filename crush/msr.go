package crush

// Порт crush_msr_do_rule из ceph/src/crush/mapper.c (Squid и новее): правила
// msr_firstn и msr_indep со шагами choosemsr. В отличие от обычных правил,
// при out OSD они повторяют весь спуск, а не только chooseleaf.

const (
	RuleTypeMSRFirstn = 4
	RuleTypeMSRIndep  = 5
)

func isMSR(t int) bool { return t == RuleTypeMSRFirstn || t == RuleTypeMSRIndep }

type msr struct {
	m          *Map
	rule       *Rule
	resultMax  int
	weight     []uint32
	x          int
	args       map[int]*ChooseArg
	descents   int
	collisions int

	// рабочее пространство EMIT-блока: вектор на каждый шаг choosemsr
	start int
	vecs  [][]int

	out      []int
	returned int
}

func (s *msr) emit(position, v int) {
	if s.rule.Type == RuleTypeMSRFirstn {
		s.out[s.returned] = v
	} else {
		s.out[position] = v
	}
	s.returned++
}

// scanNext проверяет блок TAKE CHOOSE_MSR* EMIT: ширина блока и индекс EMIT.
func (s *msr) scanNext(stepno int) (total, emit int, ok bool) {
	st := s.rule.steps
	if stepno+1 >= len(st) || st[stepno].op != "take" {
		return 0, 0, false
	}
	total = 1
	for stepno++; stepno < len(st); stepno++ {
		if st[stepno].op == "emit" {
			return total, stepno, true
		}
		if st[stepno].op != "choosemsr" {
			return 0, 0, false
		}
		if n := st[stepno].arg1; n != 0 {
			total *= n
		} else {
			total *= s.resultMax
		}
	}
	return 0, 0, false
}

func (s *msr) leafPopulated(from, to int) bool {
	leaf := s.vecs[len(s.vecs)-1]
	for i := from; i < to; i++ {
		if leaf[i] == itemUndef {
			return false
		}
	}
	return true
}

func (s *msr) retry(index, tryno, local int) int {
	total := uint32(tryno*s.resultMax + index)
	return int(total<<16 + uint32(local))
}

func (s *msr) descend(b *Bucket, typ, tryno, local, index int) int {
	for {
		var arg *ChooseArg
		if s.args != nil {
			arg = s.args[-1-b.ID]
		}
		cand := straw2Choose(b, s.x, s.retry(index, tryno, local), arg, index)
		if cand >= 0 {
			return cand
		}
		b = s.m.bucket(cand)
		if b.Type == typ {
			return cand
		}
	}
}

// valid: кандидат допустим, если уже выбран для этой полосы или не выбран ни для какой.
func (s *msr) valid(stepno, exFrom, exTo, inFrom, inTo, cand int) bool {
	vec := s.vecs[stepno-s.start]
	for i := exFrom; i < exTo; i++ {
		if vec[i] == cand {
			return i >= inFrom && i < inTo
		}
	}
	return true
}

func (s *msr) push(stepno, from, to, cand int) bool {
	vec := s.vecs[stepno-s.start]
	for i := from; i < to; i++ {
		if vec[i] == cand {
			return false
		}
		if vec[i] == itemUndef {
			vec[i] = cand
			return true
		}
	}
	return false
}

func (s *msr) pop(stepno, from, to int) {
	vec := s.vecs[stepno-s.start]
	for i := to - 1; i >= from; i-- {
		if vec[i] != itemUndef {
			vec[i] = itemUndef
			return
		}
	}
}

// choose - crush_msr_choose: заполняет позиции [from, to) шагами [cur, end).
func (s *msr) choose(b *Bucket, totalDesc, from, to, cur, end, tryno int) int {
	step := s.rule.steps[cur]
	strides := step.arg1
	if strides == 0 {
		strides = s.resultMax
	}
	strideLen := totalDesc / strides
	undo := make([]int, strides)
	for i := range undo {
		undo[i] = itemUndef
	}
	mapped := 0
	si := 0
	for ss := from; ss < to; ss, si = ss+strideLen, si+1 {
		se := min(ss+strideLen, to)
		if s.leafPopulated(ss, se) {
			continue
		}
		found, cand := false, 0
		for local := 0; local < s.collisions; local++ {
			cand = s.descend(b, step.arg2, tryno, local, si)
			if s.valid(cur, from, to, ss, se, cand) {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if step.arg2 == 0 {
			if strideLen != 1 || cur+1 != end || isOut(s.weight, cand, s.x) {
				continue
			}
			s.push(cur, ss, se, cand)
			s.emit(ss, cand)
			mapped++
			continue
		}
		if cur+1 >= end || cand >= 0 {
			continue
		}
		child := s.choose(s.m.bucket(cand), strideLen, ss, se, cur+1, end, tryno)
		if s.push(cur, ss, se, cand) && child == 0 {
			undo[si] = cand
		} else {
			mapped += child
		}
	}
	si = 0
	for ss := from; ss < to; ss, si = ss+strideLen, si+1 {
		if undo[si] != itemUndef {
			s.pop(cur, ss, min(ss+strideLen, to))
		}
	}
	return mapped
}

func (m *Map) doRuleMSR(rule *Rule, x, resultMax int, weight []uint32, args map[int]*ChooseArg) []int {
	s := &msr{m: m, rule: rule, resultMax: resultMax, weight: weight, x: x, args: args,
		descents: m.Tunables.MSRDescents, collisions: m.Tunables.MSRCollisionTries}
	if s.descents == 0 {
		s.descents = 100
	}
	if s.collisions == 0 {
		s.collisions = 100
	}
	stepno := 0
config:
	for ; stepno < len(rule.steps); stepno++ {
		switch st := rule.steps[stepno]; st.op {
		case "set_msr_descents":
			s.descents = st.arg1
		case "set_msr_collision_tries":
			s.collisions = st.arg1
		default:
			break config
		}
	}
	s.out = make([]int, resultMax)
	for i := range s.out {
		s.out[i] = ItemNone
	}
	startIndex := 0
	for stepno < len(rule.steps) {
		total, emit, ok := s.scanNext(stepno)
		if !ok {
			return nil
		}
		take := rule.steps[stepno].arg1
		if take >= 0 {
			if stepno+1 != emit || startIndex >= resultMax {
				return nil
			}
			s.emit(startIndex, take)
			startIndex++
			stepno = emit + 1
			continue
		}
		root := m.bucket(take)
		if root == nil {
			return nil
		}
		stepno++
		s.start = stepno
		s.vecs = make([][]int, emit-stepno)
		for i := range s.vecs {
			s.vecs[i] = make([]int, resultMax)
			for j := range s.vecs[i] {
				s.vecs[i][j] = itemUndef
			}
		}
		endIndex := min(startIndex+total, resultMax)
		limit := s.returned + endIndex - startIndex
		for tries := 0; tries < s.descents && s.returned < limit; tries++ {
			s.choose(root, total, startIndex, endIndex, stepno, emit, tries)
		}
		startIndex = endIndex
		stepno = emit + 1
	}
	if rule.Type == RuleTypeMSRFirstn {
		return s.out[:s.returned]
	}
	return s.out
}
