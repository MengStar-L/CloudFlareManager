import { Check, ChevronDown } from "lucide-react";
import {
  Button,
  ComboBox,
  Input,
  Label,
  ListBox,
  ListBoxItem,
  Popover,
  Select,
  SelectValue,
} from "react-aria-components";

export interface SelectOption {
  value: string;
  label: string;
  description?: string;
}

export interface SelectFieldProps {
  label: string;
  value: string;
  options: SelectOption[];
  onChange: (value: string) => void;
  name?: string;
  placeholder?: string;
  disabled?: boolean;
  required?: boolean;
  searchable?: boolean;
  className?: string;
}

function Option({ option }: { option: SelectOption }) {
  return (
    <ListBoxItem id={option.value} textValue={option.label} className="select-option">
      {({ isSelected }) => <>
        <span>{option.label}{option.description && <small>{option.description}</small>}</span>
        {isSelected && <Check size={15} aria-hidden="true" />}
      </>}
    </ListBoxItem>
  );
}

export function SelectField({
  label,
  value,
  options,
  onChange,
  name,
  placeholder = "请选择",
  disabled = false,
  required = false,
  searchable,
  className = "",
}: SelectFieldProps) {
  const useSearch = searchable ?? options.length > 8;
  const selectedKey = value || null;
  const classes = `select-field ${className}`.trim();

  return <>
    {name && <input type="hidden" name={name} value={value} />}
    {useSearch ? (
      <ComboBox
        className={classes}
        selectedKey={selectedKey}
        onSelectionChange={(key) => onChange(key === null ? "" : String(key))}
        defaultItems={options}
        defaultFilter={(textValue, inputValue) => textValue.toLocaleLowerCase().includes(inputValue.trim().toLocaleLowerCase())}
        isDisabled={disabled}
        isRequired={required}
        allowsEmptyCollection
        menuTrigger="focus"
      >
        <Label className="select-label">{label}</Label>
        <div className="combobox-control">
          <Input placeholder={placeholder} />
          <Button aria-label="展开选项"><ChevronDown size={16} aria-hidden="true" /></Button>
        </div>
        <Popover className="select-popover">
          <ListBox className="select-listbox" renderEmptyState={() => <div className="select-empty">没有匹配项</div>}>
            {(option: SelectOption) => <Option option={option} />}
          </ListBox>
        </Popover>
      </ComboBox>
    ) : (
      <Select
        className={classes}
        selectedKey={selectedKey}
        onSelectionChange={(key) => onChange(key === null ? "" : String(key))}
        isDisabled={disabled}
        isRequired={required}
        placeholder={placeholder}
      >
        <Label className="select-label">{label}</Label>
        <Button className="select-trigger">
          <SelectValue className="select-value" />
          <ChevronDown size={16} aria-hidden="true" />
        </Button>
        <Popover className="select-popover">
          <ListBox className="select-listbox" items={options}>
            {(option) => <Option option={option} />}
          </ListBox>
        </Popover>
      </Select>
    )}
  </>;
}
