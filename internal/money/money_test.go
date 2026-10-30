package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		text      string
		wantCents Cents
	}{
		{"0", 0}, {"-0.000", 0}, {"0.1", 10}, {"0.20", 20},
		{"1.001", 101}, {"1.005", 101}, {"-1.001", -101}, {"-1.005", -101},
		{"0.00001", 1}, {"1.23000", 123}, {"  +12.50 ", 1250},
		{"2e-2", 2}, {"9e-3", 1},
		{"92233720368547758.07", Cents(math.MaxInt64)},
		{"-92233720368547758.08", Cents(math.MinInt64)},
	} {
		t.Run(tc.text, func(t *testing.T) {
			gotCents, err := Parse(tc.text)
			if err != nil || gotCents != tc.wantCents {
				t.Fatalf("Parse(%q) = %d, %v; want %d", tc.text, gotCents, err, tc.wantCents)
			}
		})
	}
	for _, text := range []string{"", "NaN", "Inf", "-Infinity", "1/2", "0x10", "1_000", "1.2.3", "1e9999999", "92233720368547758.071", "-92233720368547758.081"} {
		t.Run("invalid/"+text, func(t *testing.T) {
			if _, err := Parse(text); err == nil {
				t.Fatal("accepted invalid or out-of-range money")
			}
		})
	}
}

func TestCentsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		amountCents Cents
		dollars     string
	}{
		{0, "0.00"}, {1, "0.01"}, {-1, "-0.01"}, {123456, "1234.56"},
		{Cents(math.MaxInt64), "92233720368547758.07"},
		{Cents(math.MinInt64), "-92233720368547758.08"},
	} {
		t.Run(tc.dollars, func(t *testing.T) {
			if got := tc.amountCents.String(); got != tc.dollars {
				t.Fatalf("String = %q; want %q", got, tc.dollars)
			}
			cell, err := json.Marshal(tc.amountCents.Dollars())
			if err != nil || string(cell) != tc.dollars {
				t.Fatalf("USD JSON cell = %s, %v", cell, err)
			}
			data, err := json.Marshal(tc.amountCents)
			if err != nil {
				t.Fatal(err)
			}
			var restoredCents Cents
			if err := json.Unmarshal(data, &restoredCents); err != nil || restoredCents != tc.amountCents {
				t.Fatalf("integer-cent state round trip = %d, %v", restoredCents, err)
			}
		})
	}
}

func TestCheckedArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		run       func() (Cents, error)
		wantCents Cents
		fail      bool
	}{
		{"exact addition", func() (Cents, error) { return Add(10, 20) }, 30, false},
		{"signed subtraction", func() (Cents, error) { return Subtract(-10, 20) }, -30, false},
		{"cash buy-in", func() (Cents, error) { return Multiply(10, 100) }, 1000, false},
		{"addition overflow", func() (Cents, error) { return Add(Cents(math.MaxInt64), 1) }, 0, true},
		{"addition underflow", func() (Cents, error) { return Add(Cents(math.MinInt64), -1) }, 0, true},
		{"subtraction overflow", func() (Cents, error) { return Subtract(0, Cents(math.MinInt64)) }, 0, true},
		{"subtraction underflow", func() (Cents, error) { return Subtract(Cents(math.MinInt64), 1) }, 0, true},
		{"multiplication overflow", func() (Cents, error) { return Multiply(Cents(math.MaxInt64), 100) }, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotCents, err := tc.run()
			if gotCents != tc.wantCents || errors.Is(err, ErrOverflow) != tc.fail || (!tc.fail && err != nil) {
				t.Fatalf("result = %d, %v; want %d, overflow %v", gotCents, err, tc.wantCents, tc.fail)
			}
		})
	}
}

func TestRatiosAndStrictThresholds(t *testing.T) {
	for _, tc := range []struct {
		amountCents, buyInCents Cents
		multiple                float64
		want                    bool
	}{
		{110, 100, 1.1, false}, {111, 100, 1.1, true},
		{30, 100, 0.3, false}, {31, 100, 0.3, true},
		{3, 10, 0.3, false}, {4, 10, 0.3, true},
		{100, 100, 1.001, false}, {101, 100, 1.001, true},
	} {
		if got := LossExceeds(-tc.amountCents, tc.buyInCents, tc.multiple); got != tc.want {
			t.Fatalf("%d > %d × %v = %v; want %v", tc.amountCents, tc.buyInCents, tc.multiple, got, tc.want)
		}
	}
	if got := Ratio(30, 100); got != 0.3 {
		t.Fatalf("display ratio = %v; want 0.3", got)
	}
}

func TestExtremeLossAndDifferenceStillExceedThreshold(t *testing.T) {
	if !LossExceeds(Cents(math.MinInt64), 100, 1) {
		t.Fatal("negating minimum amount hid a loss")
	}
	if !DifferenceExceeds(Cents(math.MaxInt64), Cents(math.MinInt64), 100, 1) {
		t.Fatal("drawdown subtraction hid a loss")
	}
}
