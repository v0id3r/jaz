import { expect, test } from 'bun:test'
import { pickerEffortOptions, parseModelSelections } from './modelPicker'

const options = (values) => values.map((value) => ({ value, label: value }))

test('picker keeps every advertised effort in ascending order without changing provider IDs', () => {
  const available = options(['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultracode'])
  expect(pickerEffortOptions(available).map(({ value }) => value)).toEqual(['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultracode'])
  expect(available).toHaveLength(9)
  expect(pickerEffortOptions(options(['ultra', 'max', 'xhigh', 'high', 'medium', 'low']))).toEqual([
    { value: 'low', label: 'low' },
    { value: 'medium', label: 'medium' },
    { value: 'high', label: 'high' },
    { value: 'xhigh', label: 'Xhigh' },
    { value: 'max', label: 'max' },
    { value: 'ultra', label: 'Ultracode' },
  ])
  expect(pickerEffortOptions(options(['ultra', 'ultracode']))).toEqual([
    { value: 'ultra', label: 'Ultracode' },
    { value: 'ultracode', label: 'Ultracode' },
  ])
})

test('picker offers Kimi-style low/high/max and skips the empty default', () => {
  expect(pickerEffortOptions(options(['max', 'high', 'low'])).map(({ value }) => value)).toEqual(['low', 'high', 'max'])
  expect(pickerEffortOptions(options(['', 'high', 'max']))).toEqual([
    { value: 'high', label: 'high' },
    { value: 'max', label: 'max' },
  ])
  expect(pickerEffortOptions([])).toEqual([])
})

test('saved picker choices keep agents and providers separate and discard obsolete modes', () => {
  const selections = {
    'codex/openai': { model: 'gpt-6-astra', effort: 'medium', fastMode: true },
    'codex/openrouter': { model: 'qwen/qwen3', effort: '', fastMode: false },
    'claude/': { model: 'opus[1m]', effort: 'ultracode' },
  }
  expect(parseModelSelections(JSON.stringify(selections))).toEqual(selections)
  expect(parseModelSelections(JSON.stringify({ 'claude/': { ...selections['claude/'], mode: 'recommended' } }))).toEqual({ 'claude/': selections['claude/'] })
  for (const raw of [null, 'null', '{bad json', '{"claude/":null}', '{"codex/openai":{"model":2}}']) {
    expect(parseModelSelections(raw)).toEqual({})
  }
})
