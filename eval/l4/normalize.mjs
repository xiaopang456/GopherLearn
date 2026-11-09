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
// The table was measured, not guessed: see the census over the four frozen sets
// (tutorial/gopherlearn-optimization/l4-measure-word-census.json). "个" is deliberately
// NOT in it. It was measured in five places, all of them `required` values of
// needs_judge scenarios, which this file never compares; adding it would change
// nothing.

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

// chineseNumber reads a Chinese numeral. It returns NaN for anything it cannot read,
// and the caller then keeps the value as it was written instead of guessing.
//
// The unit matters: a plain "十" is ten, "二十" is twenty, and "一千二百" is twelve
// hundred. "万" is a section: "两万五千" is twenty-five thousand, not twenty-two
// thousand and five.
export function chineseNumber(s) {
  if (/^\d+(?:\.\d+)?$/.test(s)) return Number(s)
  let total = 0, section = 0, digit = 0
  for (const c of s) {
    if (c === '万' || c === '萬') { total += (section + digit) * 10000; section = 0; digit = 0 }
    else if (c === '千') { section += (digit || 1) * 1000; digit = 0 }
    else if (c === '百') { section += (digit || 1) * 100; digit = 0 }
    else if (c === '十') { section += (digit || 1) * 10; digit = 0 }
    else if (Object.hasOwn(DIGITS, c)) digit = digit * 10 + DIGITS[c]
    else return NaN
  }
  return total + section + digit
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

// fold is the presentation folding every value goes through before it is compared:
// full-width characters become half-width, spacing disappears, a trailing sentence
// mark is dropped, and the result is lower-cased.
export function fold(s) {
  return String(s ?? '').normalize('NFKC').replace(/\s+/gu, '').replace(/[。.!！]$/u, '').toLowerCase()
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
