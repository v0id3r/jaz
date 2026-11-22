import type { ReasoningEffortOption } from '@/lib/api/types'
import type { ModelSuggestion } from '@/lib/modelSuggestion'

export type ModelPickerOption = Pick<ModelSuggestion, 'value' | 'label' | 'aliases'> & { reasoning?: ModelSuggestion['reasoning'] }

export interface ModelSelection {
  model: string
  effort: string
  fastMode?: boolean
}

const EFFORT_ORDER = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra', 'ultracode']

export function pickerEffortOptions(options: ReasoningEffortOption[]): ReasoningEffortOption[] {
  return options
    .filter((option) => option.value !== '')
    .map((option) => ({ ...option, label: pickerEffortLabel(option) }))
    .sort((a, b) => {
      const ia = EFFORT_ORDER.indexOf(a.value)
      const ib = EFFORT_ORDER.indexOf(b.value)
      return (ia === -1 ? EFFORT_ORDER.length : ia) - (ib === -1 ? EFFORT_ORDER.length : ib)
    })
}

function pickerEffortLabel(option: ReasoningEffortOption): string {
  switch (option.value) {
    case 'xhigh':
      return 'Xhigh'
    case 'ultra':
    case 'ultracode':
      return 'Ultracode'
    default:
      return option.label
  }
}

export function parseModelSelections(raw: string | null): Record<string, ModelSelection> {
  try {
    const parsed = JSON.parse(raw ?? '{}')
    return Object.fromEntries(Object.entries(parsed).flatMap(([key, entry]) => {
      const value = entry as Partial<ModelSelection> | null
      return value != null && typeof value.model === 'string' && typeof value.effort === 'string'
        ? [[key, { model: value.model, effort: value.effort, ...(typeof value.fastMode === 'boolean' ? { fastMode: value.fastMode } : {}) }]] : []
    }))
  } catch {
    return {}
  }
}
