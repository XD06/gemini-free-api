from langchain_anthropic import ChatAnthropic

# Initialize the client pointing to our local bridge
llm = ChatAnthropic(
    base_url="http://localhost:8787/claude", 
    model="gemini-advanced",
    temperature=0.7,
    api_key="abc"
)
response = llm.invoke("Can you introduce yourself?")
print(response.content)
