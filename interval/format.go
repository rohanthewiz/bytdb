package interval

// format.go: interval output in Postgres's default IntervalStyle,
// "postgres" (EncodeInterval):
//
//	1 year 2 mons 3 days 04:05:06.789
//	-1 days +02:00:00
//	00:00:00
//
// Years, months and days print as counted words ("mon" is singular,
// "mons" plural, for compatibility; zero fields are left out). The clock
// prints as [-]hh:mm:ss[.ffffff] when it is nonzero or nothing else
// printed, with at least two hour digits and no trailing fraction zeros.
// A sign rule keeps the text re-readable: after a negative field, the
// next field carries an explicit '+' when it is positive — 'ago'-free
// input reads each field's sign on its own, so '-1 days +02:00:00'
// round-trips where '-1 days 02:00:00' would too, but '-1 days 2 hours'
// style ambiguity never arises.

import "strconv"

// String renders the interval as Postgres's interval_out does.
func (iv Interval) String() string { return string(iv.AppendText(nil)) }

// AppendText appends the interval's text to buf.
func (iv Interval) AppendText(buf []byte) []byte {
	// interval2itm: years and months from the month count, the clock
	// split into hours, minutes, seconds and microseconds, every part
	// truncated toward zero so each keeps the field's sign.
	year := int64(iv.Months / MonthsPerYear)
	mon := int64(iv.Months % MonthsPerYear)
	t := iv.Micros
	hour := t / MicrosPerHour
	t -= hour * MicrosPerHour
	min := t / MicrosPerMinute
	t -= min * MicrosPerMinute
	sec := t / MicrosPerSecond
	fsec := t - sec*MicrosPerSecond

	isZero, isBefore := true, false
	part := func(v int64, unit string) {
		if v == 0 {
			return
		}
		if !isZero {
			buf = append(buf, ' ')
		}
		if isBefore && v > 0 {
			buf = append(buf, '+')
		}
		buf = strconv.AppendInt(buf, v, 10)
		buf = append(buf, ' ')
		buf = append(buf, unit...)
		if v != 1 {
			buf = append(buf, 's')
		}
		// Each nonzero field decides the '+' of the next one only.
		isBefore = v < 0
		isZero = false
	}
	part(year, "year")
	part(mon, "mon")
	part(int64(iv.Days), "day")
	if isZero || hour != 0 || min != 0 || sec != 0 || fsec != 0 {
		minus := hour < 0 || min < 0 || sec < 0 || fsec < 0
		if !isZero {
			buf = append(buf, ' ')
		}
		switch {
		case minus:
			buf = append(buf, '-')
		case isBefore:
			buf = append(buf, '+')
		}
		buf = appendPadded(buf, uabs(hour), 2)
		buf = append(buf, ':')
		buf = appendPadded(buf, uabs(min), 2)
		buf = append(buf, ':')
		buf = appendPadded(buf, uabs(sec), 2)
		if fsec != 0 {
			buf = appendFraction(buf, uabs(fsec))
		}
	}
	return buf
}

// appendPadded appends v in decimal with at least width digits.
func appendPadded(buf []byte, v uint64, width int) []byte {
	s := strconv.FormatUint(v, 10)
	for i := len(s); i < width; i++ {
		buf = append(buf, '0')
	}
	return append(buf, s...)
}

// appendFraction appends ".ffffff" for a microsecond count (1..999999),
// without trailing zeros.
func appendFraction(buf []byte, us uint64) []byte {
	var d [6]byte
	for i := 5; i >= 0; i-- {
		d[i] = byte('0' + us%10)
		us /= 10
	}
	n := 6
	for n > 0 && d[n-1] == '0' {
		n--
	}
	buf = append(buf, '.')
	return append(buf, d[:n]...)
}
