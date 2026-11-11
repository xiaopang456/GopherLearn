// One measure-word table, one normalization.
//
// The scorer looks for a value inside an answer, and the answer key compares its own
// values against each other. Both have to agree on what "the same value written
// differently" means, so both call the functions in this file. The table of measure
// words lives here as well, and only here: the scanner that finds mentions in prose
// and the canonical form that compares them are built from the same list, so a word
// cannot be known to one and unknown to the other.
//
// What the table holds:
//
//   - the measure words the frozen answer keys actually use;
//   - the duration words and their English spellings, because the answer may use them
//     even when the key does not.
//
// The table was measured, not guessed: a census over the four frozen question sets
// counted the measure words the answer keys really use. The census and its report are
// not in this repository. "个" is deliberately NOT in the table. It was measured in
// five places, all of them `required` values of needs_judge scenarios, which this
// file never compares; adding it would change nothing.

// The order matters for the scanner: a longer spelling comes before a shorter one
// that starts with it.
export const MEASURE_WORDS = [
  'minutes', 'minute', 'seconds', 'second', 'hours', 'hour',
  'mins', 'min', 'secs', 'sec', 'hrs', 'hr',
  '分钟', '分鐘', '小时', '小時', '秒钟', '秒鐘', '页', '頁',
  '分', '时', '時', '秒', '次', '天', '道',
  'h', 's',
]

// The measure words that are a length of time. They all become the same quantity -
// seconds - so that "38分钟" and "2280秒" are the same value.
export const DURATION_WORDS = new Set([
  '分钟', '分鐘', '分', 'minute', 'minutes', 'min', 'mins',
  '小时', '小時', '时', '時', 'hour', 'hours', 'hr', 'hrs', 'h',
  '秒', '秒钟', '秒鐘', 'second', 'seconds', 'sec', 'secs', 's',
])

// How many seconds one of a duration word is worth.
const DURATION_FACTOR = new Map([
  ['分钟', 60n], ['分鐘', 60n], ['分', 60n], ['minute', 60n], ['minutes', 60n], ['min', 60n], ['mins', 60n],
  ['小时', 3600n], ['小時', 3600n], ['时', 3600n], ['時', 3600n], ['hour', 3600n], ['hours', 3600n], ['hr', 3600n], ['hrs', 3600n], ['h', 3600n],
  ['秒', 1n], ['秒钟', 1n], ['秒鐘', 1n], ['second', 1n], ['seconds', 1n], ['sec', 1n], ['secs', 1n], ['s', 1n],
])

// The measure words that are a plain count. Everything else in the table is a
// duration.
export const COUNT_WORDS = MEASURE_WORDS.filter(word => !DURATION_WORDS.has(word))

// A measure word followed by one of these characters is part of an ordinary word and
// not a count of anything: "次元", "天气", "道路". Such a mention is ambiguous and
// does not establish a fact. The guard lives next to the table so that a new measure
// word brings its compounds with it.
export const COMPOUND_AFTER = new Map([
  ['次', ['元', '方', '数', '數']],
  ['天', ['气', '氣', '然', '赋', '賦', '线', '線']],
  ['道', ['路', '知', '德', '理', '轨', '軌', '渠', '味']],
])

// compoundMention reports whether a mention is part of an ordinary word rather than a
// count: "9道路" is not nine roads.
export function compoundMention(expression, after) {
  for (const [word, followers] of COMPOUND_AFTER) {
    if (!expression.endsWith(word)) continue
    if (followers.some(character => after.startsWith(character))) return true
  }
  return false
}

const DIGITS = { 零: 0, 〇: 0, 一: 1, 二: 2, 两: 2, 兩: 2, 三: 3, 四: 4, 五: 5, 六: 6, 七: 7, 八: 8, 九: 9 }

// The units of a section (the part below ten thousand) and what they are worth.
const SECTION_UNITS = new Map([['十', 10], ['百', 100], ['千', 1000]])

// parseSection reads the part of a Chinese numeral that is below ten thousand.
//
// Only a legal writing is read. The units have to come in decreasing order and may not
// repeat, only "十" may leave out the digit in front of it, and two digits may not
// stand next to each other. "一百百" is therefore not read (as a hundred hundred, or
// as two hundred - it is not either), and "一二" is not read as twelve.
//
// A "零" in the middle fills the place of a digit that is not written ("一百零八"), so
// the digit it stands for has to follow it.
//
// It returns null for anything it cannot read, and the caller then keeps the value as
// it was written instead of guessing.
function parseSection(text) {
  if (!text) return null
  let total = 0
  let digit = null
  let digitWasZero = false
  let zeroPending = false
  let sawUnit = false
  let smallestUnit = 10000
  for (const c of text) {
    if (c === '零' || c === '〇') {
      if (!sawUnit) {
        // A leading zero: "零五" is five, and "零" on its own is zero.
        if (digit !== null) return null
        digit = 0
        digitWasZero = true
        continue
      }
      // A zero in the middle fills the place of a digit that is not written, and the
      // digit it stands for has to follow it.
      //
      // It may only fill a place that is empty: a digit that was written before it
      // and not attached to a unit yet is still waiting for its unit, and a zero on
      // top of it would drop that digit. "一百二零三" is therefore not read (as a
      // hundred and three, which would lose the two), and "一百零三" is.
      if (zeroPending || digit !== null) return null
      zeroPending = true
      continue
    }
    if (Object.hasOwn(DIGITS, c)) {
      if (zeroPending) { zeroPending = false; digit = DIGITS[c]; digitWasZero = false; continue }
      if (digit !== null) {
        // Two digits in a row are not a number.
        if (!digitWasZero) return null
        digit = DIGITS[c]
        digitWasZero = false
        continue
      }
      digit = DIGITS[c]
      digitWasZero = false
      continue
    }
    const unit = SECTION_UNITS.get(c)
    if (unit === undefined) return null
    if (unit >= smallestUnit) return null
    if (zeroPending) return null
    smallestUnit = unit
    // Only "十" may leave out the digit in front of it ("十" is ten). "百" and "千"
    // have to be written out ("一百", "一千"), so "千十" is not read as a number.
    if (digit === null && c !== '十') return null
    total += (digit === null ? 1 : digit) * unit
    sawUnit = true
    digit = null
    digitWasZero = false
  }
  // A zero that waited for a digit never got one.
  if (zeroPending) return null
  if (digit !== null) total += digit
  return total
}

// chineseNumber reads a Chinese numeral. It returns NaN for anything it cannot read,
// and the caller then keeps the value as it was written instead of guessing.
//
// Only a complete, legal writing is read. A single "万", a "万万" and a "一百百" are
// not numbers, and they are not read as zero either: a value that is not read stays
// the text it was written as and is matched literally, so a key of "万次" can never
// be satisfied by an answer of "0次".
//
// The unit matters: a plain "十" is ten, "二十" is twenty, and "一千二百" is twelve
// hundred. "万" is a section: "两万五千" is twenty-five thousand, not twenty-two
// thousand and five.
export function chineseNumber(s) {
  if (/^\d+(?:\.\d+)?$/.test(s)) return Number(s)
  if (typeof s !== 'string' || !s) return NaN
  const sections = s.split(/[万萬]/)
  if (sections.length > 2) return NaN
  if (sections.length === 1) {
    const value = parseSection(s)
    return value === null ? NaN : value
  }
  const [high, low] = sections
  // "万" alone, and "万五千", are not legal writings: the ten-thousands part has to be
  // written out.
  if (!high) return NaN
  const highValue = parseSection(high)
  if (highValue === null) return NaN
  if (low === '') return highValue * 10000
  if (/^[零〇]$/.test(low)) return NaN
  // The part below the ten-thousands has to say what it is: a unit ("两万五千"), or a
  // zero that shows the place is empty ("一万零二"). A bare digit right after the
  // ten-thousands ("一万二") is left out, because it is read as ten thousand and two
  // here and as twelve thousand in speech - two values for one writing.
  if (!/[十百千]/.test(low) && !/^[零〇]/.test(low)) return NaN
  const lowValue = parseSection(low)
  if (lowValue === null) return NaN
  return highValue * 10000 + lowValue
}

// The number a quantity may carry: Arabic digits, or Chinese numerals.
const NUM = '(?:[0-9]+(?:\\.[0-9]+)?|[零〇一二两兩三四五六七八九十百千万萬]+)'

// The scanner: a number with a measure word, anywhere in a text. The unit part is
// built from the one table.
const UNIT_ALTERNATION = MEASURE_WORDS.map(word => word.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')

// The measure words, as one alternation. Exported so the scanner and the canonical
// form cannot drift apart.
export const QUANTITY_PATTERN = `(?:${NUM})(?:${UNIT_ALTERNATION})`

// The mention scanner: a clock expression or a quantity, anywhere in a text.
export const MENTION_EXPRESSION = new RegExp(
  `(?:凌晨|早上|上午|中午|下午|晚上|傍晚)?(?:${NUM})(?:[:：]\\d{1,2}|点(?:半|一刻|三刻|${NUM}分?)?)|${QUANTITY_PATTERN}`,
  'giu',
)

// A value that is exactly one clock expression.
const CLOCK_EXPRESSION = new RegExp(`^(凌晨|早上|上午|中午|下午|晚上|傍晚)?(${NUM})(?:[:](\\d{1,2})|点(半|一刻|三刻|${NUM}分?)?)$`, 'u')

// A value that is exactly one quantity.
const QUANTITY_EXPRESSION = new RegExp(`^(${NUM})(${UNIT_ALTERNATION})$`, 'iu')

// The one folding of a text.
//
// A character is normalized to its half-width form and lower-cased, the spacing
// is dropped, and every character keeps the index it had in the text it came
// from. The indices are what lets the scorer point at a mention in the answer.
//
// Three things are folded with it: the answer the scorer looks a value in, the
// values of the answer key (the accepted values, the aliases and the forbidden
// values alike), and the history the second round's checks read a value out of.
// They are folded the same way, so that a value and the text that states it are
// written the same way on both sides.
//
// The scorer's normalization answers a different question and is not this one:
// it names a quantity canonically ("38 minutes" and "2280 seconds" are one
// value) instead of folding a whole sentence. A value that is not an
// expression is looked for literally in the folded text, which is what the
// datasets were written against.
export function foldCharacters(text) {
  const base = String(text ?? '').normalize('NFKC')
  let out = ''
  const map = []
  for (let i = 0; i < base.length; i += 1) {
    if (/\s/u.test(base[i])) continue
    for (const character of base[i].toLowerCase()) {
      out += character
      map.push(i)
    }
  }
  return { text: out, map }
}

// foldText is the same folding as one string: the full-width characters become
// half-width, the spacing disappears and the result is lower-cased.
export function foldText(text) {
  return foldCharacters(text).text
}

// fold is the folding of a value: the same folding, and a sentence mark at the
// end of the value is not part of it.
export function fold(s) {
  return foldText(s).replace(/[。.!！]$/u, '')
}

// A rational number, as the lowest terms. A duration is compared as an exact
// fraction, so that "0.5分钟" and "30秒" are the same value and "0.05分钟" is
// exactly three seconds and not three and a bit.
function rational(numerator, denominator) {
  let a = numerator, b = denominator
  while (b) [a, b] = [b, a % b]
  return `${numerator / a}/${denominator / a}`
}

// The number of a quantity, as an exact fraction. A Chinese numeral that cannot be
// read is refused: the caller keeps the value as it was written.
function quantityNumber(token) {
  if (/^\d+(?:\.\d+)?$/.test(token)) {
    const [integer, fraction = ''] = token.split('.')
    return { numerator: BigInt(integer + fraction), denominator: 10n ** BigInt(fraction.length) }
  }
  const value = chineseNumber(token)
  if (!Number.isFinite(value)) return null
  return { numerator: BigInt(value), denominator: 1n }
}

// canonicalExpression turns a value that IS one expression into its canonical form:
//
//   clock:h:m        a clock time
//   seconds:N/M      a length of time, in seconds, as a fraction
//   quantity:N:unit  a count of things
//
// It returns null when the value is not an expression, or when the number in it cannot
// be read. A value it refuses is not guessed at: the caller keeps it as it was
// written and matches it literally.
export function canonicalExpression(s) {
  const t = fold(s)
  const clock = CLOCK_EXPRESSION.exec(t)
  if (clock) {
    let h = chineseNumber(clock[2])
    const m = clock[3] === undefined
      ? (clock[4] === '半' ? 30 : clock[4] === '一刻' ? 15 : clock[4] === '三刻' ? 45 : clock[4] ? chineseNumber(clock[4].replace(/分$/, '')) : 0)
      : Number(clock[3])
    const period = clock[1]
    if (period && (h < 1 || h > 12)) return null
    if (['下午', '晚上', '傍晚'].includes(period) && h < 12) h += 12
    if (['凌晨', '早上', '上午'].includes(period) && h === 12) h = 0
    if (period === '中午' && h < 11) h += 12
    if (!Number.isFinite(h) || !Number.isFinite(m)) return null
    if (h > 23 || m > 59) return null
    return `clock:${h}:${m}`
  }
  const quantity = QUANTITY_EXPRESSION.exec(t)
  if (!quantity) return null
  const number = quantityNumber(quantity[1])
  if (!number) return null
  const unit = quantity[2]
  const factor = DURATION_FACTOR.get(unit.toLowerCase())
  if (factor) return `seconds:${rational(number.numerator * factor, number.denominator)}`
  const count = number.denominator === 1n ? number.numerator.toString() : rational(number.numerator, number.denominator)
  return `quantity:${count}:${unit === '頁' ? '页' : unit}`
}

// normalizeValue is the one normalization. It returns the canonical form of the
// value, or `text:` plus the folded value when the value is not an expression.
//
// The scorer calls it on the answer key's values and on every mention it finds in an
// answer; the answer key's own comparison calls it on accepted, aliases and
// forbidden. That is what makes "9道" and "九道" the same key, and the same value in
// an answer.
export function normalizeValue(s) {
  return canonicalExpression(s) ?? `text:${fold(s)}`
}

// The canonical form of a value that is a length of time, in seconds. This is what
// the scorer has always answered for a duration, and it stays: "38分钟" and
// "2280秒" are the same value.
export function normalizeFact(s) {
  const text = fold(s)
  const quantity = new RegExp(`^(\\d+(?:\\.\\d+)?)(${UNIT_ALTERNATION})$`, 'u').exec(text)
  if (!quantity) return text
  const number = quantityNumber(quantity[1])
  if (!number) return text
  const factor = DURATION_FACTOR.get(quantity[2].toLowerCase())
  if (!factor) return text
  return `seconds:${rational(number.numerator * factor, number.denominator)}`
}
