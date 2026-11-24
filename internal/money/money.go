package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

type Cents int64

var ErrOverflow = errors.New("USD cents overflow")

func Parse(dollars string) (Cents, error) {
	text := strings.TrimSpace(dollars)
	if len(text) > 128 || strings.IndexFunc(text, func(r rune) bool {
		return !strings.ContainsRune("0123456789.+-eE", r)
	}) >= 0 {
		return 0, fmt.Errorf("invalid USD amount %q", dollars)
	}
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(text[index+1:])
		if err != nil || exponent < -128 || exponent > 128 {
			return 0, fmt.Errorf("USD exponent out of range")
		}
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok || strings.Contains(text, "/") {
		return 0, fmt.Errorf("invalid USD amount %q", dollars)
	}
	value.Mul(value, big.NewRat(100, 1))
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() != 0 {
		whole.Add(whole, big.NewInt(int64(value.Sign())))
	}
	if !whole.IsInt64() {
		return 0, fmt.Errorf("USD amount %q: %w", dollars, ErrOverflow)
	}
	return Cents(whole.Int64()), nil
}

func Add(leftCents, rightCents Cents) (Cents, error) {
	if (rightCents > 0 && leftCents > Cents(math.MaxInt64)-rightCents) ||
		(rightCents < 0 && leftCents < Cents(math.MinInt64)-rightCents) {
		return 0, ErrOverflow
	}
	return leftCents + rightCents, nil
}

func Subtract(leftCents, rightCents Cents) (Cents, error) {
	if (rightCents < 0 && leftCents > Cents(math.MaxInt64)+rightCents) ||
		(rightCents > 0 && leftCents < Cents(math.MinInt64)+rightCents) {
		return 0, ErrOverflow
	}
	return leftCents - rightCents, nil
}

func Multiply(amountCents Cents, count int64) (Cents, error) {
	result := new(big.Int).Mul(big.NewInt(int64(amountCents)), big.NewInt(count))
	if !result.IsInt64() {
		return 0, ErrOverflow
	}
	return Cents(result.Int64()), nil
}

func (cCents Cents) String() string {
	negative := cCents < 0
	magnitude := uint64(cCents)
	if negative {
		magnitude = uint64(-(cCents + 1)) + 1
	}
	fraction := strconv.FormatUint(magnitude%100, 10)
	if len(fraction) == 1 {
		fraction = "0" + fraction
	}
	result := strconv.FormatUint(magnitude/100, 10) + "." + fraction
	if negative {
		result = "-" + result
	}
	return result
}

func (cCents Cents) Dollars() json.Number { return json.Number(cCents.String()) }

func Ratio(numeratorCents, denominatorCents Cents) float64 {
	if denominatorCents == 0 {
		return 0
	}
	ratio := new(big.Rat).SetFrac(big.NewInt(int64(numeratorCents)), big.NewInt(int64(denominatorCents)))
	value, _ := ratio.Float64()
	return value
}

func LossExceeds(netCents, buyInCents Cents, multiplier float64) bool {
	lossCentsRat := new(big.Rat).Neg(new(big.Rat).SetInt64(int64(netCents)))
	return exceedsMultiple(lossCentsRat, buyInCents, multiplier)
}

func DifferenceExceeds(peakCents, currentCents, buyInCents Cents, multiplier float64) bool {
	differenceCentsRat := new(big.Rat).Sub(new(big.Rat).SetInt64(int64(peakCents)), new(big.Rat).SetInt64(int64(currentCents)))
	return exceedsMultiple(differenceCentsRat, buyInCents, multiplier)
}

func exceedsMultiple(amountCentsRat *big.Rat, buyInCents Cents, multiplier float64) bool {
	ratio, ok := new(big.Rat).SetString(strconv.FormatFloat(multiplier, 'g', -1, 64))
	if !ok {
		return false
	}
	thresholdCentsRat := ratio.Mul(ratio, new(big.Rat).SetInt64(int64(buyInCents)))
	return amountCentsRat.Cmp(thresholdCentsRat) > 0
}
