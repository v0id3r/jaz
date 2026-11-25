import type { AgentSessionConfigOption } from '@/lib/api/types'

export interface FastModeControl {
  checked: boolean
  disabled?: boolean
  onChange: (checked: boolean) => void
}

export function fastModeOption(options?: AgentSessionConfigOption[] | null) {
  return options?.find((option) =>
    (option.category === 'model_config' || option.category === 'speed') &&
    option.name.toLowerCase() === 'fast mode' &&
    (option.type === 'boolean' ||
      (option.options.length === 2 &&
        option.options.some((value) => value.value === 'on') &&
        option.options.some((value) => value.value === 'off'))),
  )
}
